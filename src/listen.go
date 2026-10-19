package main

// listen.go -- listen mode: the pads and buttons stay Live's, and the
// screen names the chord being played.
//
// On Push the pads reach the selected track through Push's own path; no
// MIDI input carries them. So Browser Bridge (0.4) keeps a silent track,
// "Stacks Listen", whose input follows the selected track and whose output
// is this port, and Stacks hears what that track plays -- the pitches Live
// plays, whatever Push's layout or octave. These notes never reach the
// core: they are only named and drawn.

import (
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/rgeissen/push-hack-stacks/dsp"
	"github.com/rgeissen/push-hack-stacks/surface"
)

const (
	portListen  = "Stacks Listen"
	listenEvery = 2 * time.Second // re-assert the track, and read what it listens to
	listenKeep  = 400 * time.Millisecond
)

// listenState is what is being played, for the screen. The MIDI reader
// writes it; the screen reads it.
type listenState struct {
	mu     sync.Mutex
	notes  map[int]*surface.ListenNote
	source string
	status string

	// Screen goroutine only: the name and when it changed.
	name, prev string
	nameAt     time.Time
	named      string // the held notes the name is for
	root, bass int
	inv        int
	known      bool
	interval   string
	omit       []int
	keysLo     int
	unnamed    map[string]bool // sets nothing fits, logged once each

	// The chord being played, and the one played before it (LAST). A chord
	// is finished when every note is up, or when notes are added that make
	// a different chord; lifting notes alone does not change it.
	cur, last heardChord
	cleared   int // seen value of clears
	clears    int // bumped by clear (any goroutine, under mu)
}

// heardChord is a chord the board named, for LAST.
type heardChord struct {
	name       string
	root, bass int
	known      bool
	held       string // its notes, "60.64.67"
}

func newListenState() *listenState {
	return &listenState{notes: map[int]*surface.ListenNote{}, root: -1}
}

// Fixed takes the listen port's events: note on and off, any channel.
func (l *listenState) Fixed(evType uint8, _ alsaseq.Addr, data []byte) {
	msg := alsaseq.DecodeMIDI(evType, data)
	if len(msg) < 3 {
		return
	}
	note, on := int(msg[1]), msg[0]&0xF0 == 0x90 && msg[2] > 0
	off := msg[0]&0xF0 == 0x80 || msg[0]&0xF0 == 0x90 && msg[2] == 0
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case on:
		l.notes[note] = &surface.ListenNote{Note: note, On: now}
	case off:
		if n, ok := l.notes[note]; ok && n.Off.IsZero() {
			n.Off = now
		}
	}
}

func (*listenState) VarLen(uint8, alsaseq.Addr, []byte) {}

// snapshot returns the notes to draw, dropping released ones that have
// faded, and the held ones as the core's key ("52.60.64").
func (l *listenState) snapshot(now time.Time) (notes []surface.ListenNote, held string, source, status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var hs []int
	for k, n := range l.notes {
		if !n.Off.IsZero() && now.Sub(n.Off) > listenKeep {
			delete(l.notes, k)
			continue
		}
		notes = append(notes, *n)
		if n.Off.IsZero() {
			hs = append(hs, n.Note)
		}
	}
	sort.Ints(hs)
	parts := make([]string, len(hs))
	for i, n := range hs {
		parts[i] = strconv.Itoa(n)
	}
	return notes, strings.Join(parts, "."), l.source, l.status
}

func (l *listenState) clear() {
	l.mu.Lock()
	l.notes = map[int]*surface.ListenNote{}
	l.clears++ // the screen goroutine forgets the board and LAST
	l.mu.Unlock()
}

func (l *listenState) setTrack(source, status string) {
	l.mu.Lock()
	l.source, l.status = source, status
	l.mu.Unlock()
}

// scene builds a frame: the chord named by the core (chord_of), once per
// change of the held notes.
func (l *listenState) scene(eng *Engine, now time.Time, key, mask int) surface.ListenScene {
	notes, held, source, status := l.snapshot(now)
	l.mu.Lock()
	clears := l.clears
	l.mu.Unlock()
	if clears != l.cleared { // listening began or ended: a clean board
		l.cleared = clears
		l.cur, l.last, l.named = heardChord{}, heardChord{}, ""
		l.show(chordOf0(), now)
	}
	l.hear(held, now, func(held string) chordOf {
		var reply string
		eng.Call(func(i *dsp.Instance) { reply, _ = i.GetParam("chord_of:" + held) }, 200*time.Millisecond)
		return parseChordOf(reply)
	})
	l.keysLo = surface.KeyWindow(l.keysLo, notes)
	last := ""
	if l.last.name != "" {
		last = surface.BoardText(l.last.name, l.last.root, l.last.bass, l.last.known)
	}
	return surface.ListenScene{Now: now, Notes: notes, Name: l.name, PrevName: l.prev, NameAt: l.nameAt,
		Root: l.root, Bass: l.bass, Inv: l.inv, Known: l.known, Interval: l.interval, Omit: l.omit,
		Last: last, LastRoot: l.last.root,
		Key: key, Mask: mask, KeysLo: l.keysLo, Source: source, Status: status}
}

// hear updates the board and LAST for the notes held now.
//
//   - Every note up: the board is blank at once, and the chord goes to
//     LAST. (It used to keep the name while the released notes faded, and
//     as nothing changed after they had, it kept it for good: reported on
//     device, "the display shows the last chord even if no chord is
//     pressed".)
//   - A note added: the board names what is held. A chord that is not the
//     same one growing (its notes no longer all held) goes to LAST.
//   - Notes lifted, some still held: the board keeps the chord, instead of
//     flipping through every shrinking subset.
func (l *listenState) hear(held string, now time.Time, chordOf func(string) chordOf) {
	if held == l.named {
		return
	}
	before := l.named
	l.named = held
	switch {
	case held == "":
		if l.cur.name != "" {
			l.last, l.cur = l.cur, heardChord{}
		}
		l.show(chordOf0(), now)
	case !addsNote(before, held):
		// lifted only: the board stays
	default:
		c := chordOf(held)
		if c.kind == 2 { // two notes: say them, and the interval between
			ns := strings.Split(held, ".")
			lo, _ := strconv.Atoi(ns[0])
			hi, _ := strconv.Atoi(ns[len(ns)-1])
			c.name = surface.NoteName(lo) + "-" + surface.NoteName(hi)
			c.interval = surface.IntervalWords[((hi-lo)%12+12)%12]
		}
		if c.kind == 0 && !l.unnamed[held] {
			if l.unnamed == nil {
				l.unnamed = map[string]bool{}
			}
			l.unnamed[held] = true
			log.Printf("listen: no chord fits %s (notes, bass first)", held)
		}
		if c.kind == 1 {
			if l.cur.name != "" && !subsetOf(l.cur.held, held) {
				l.last = l.cur // a new chord, not this one growing
			}
			l.cur = heardChord{name: c.name, root: c.root, bass: c.bass, known: true, held: held}
		}
		l.show(c, now)
	}
}

func chordOf0() chordOf { return chordOf{root: -1, bass: -1} }

// show puts c on the board; the flaps flip only when the text changes.
func (l *listenState) show(c chordOf, now time.Time) {
	if c.name == l.name && c.inv == l.inv && c.bass == l.bass && c.interval == l.interval {
		return
	}
	prev := surface.BoardText(l.name, l.root, l.bass, l.known)
	if l.interval != "" {
		prev = l.name
	}
	l.prev, l.name, l.nameAt = prev, c.name, now
	l.root, l.bass, l.inv, l.known = c.root, c.bass, c.inv, c.kind == 1
	l.interval, l.omit = c.interval, c.omit
}

// addsNote is whether held has a note that was not in before.
func addsNote(before, held string) bool {
	return !subsetOf(held, before)
}

// subsetOf is whether every note of a is in b ("60.64" in "60.64.67").
func subsetOf(a, b string) bool {
	if a == "" {
		return true
	}
	in := map[string]bool{}
	for _, n := range strings.Split(b, ".") {
		in[n] = true
	}
	for _, n := range strings.Split(a, ".") {
		if !in[n] {
			return false
		}
	}
	return true
}

// chordOf is the core's answer for a set of notes (chord_of).
type chordOf struct {
	name       string
	root, bass int
	inv        int // -1: a bass outside the chord
	kind       int // 1 a chord, 2 an interval, 0 nothing fits
	omit       []int
	interval   string // set by the caller for an interval
}

// parseChordOf reads NAME|ROOT|INV|BASS|KNOWN|OMIT (an older core sends
// the first five).
func parseChordOf(s string) chordOf {
	f := strings.Split(s, "|")
	if len(f) < 5 {
		return chordOf{root: -1, bass: -1}
	}
	c := chordOf{name: f[0]}
	c.root, _ = strconv.Atoi(f[1])
	c.inv, _ = strconv.Atoi(f[2])
	c.bass, _ = strconv.Atoi(f[3])
	c.kind, _ = strconv.Atoi(f[4])
	if len(f) > 5 && f[5] != "-" {
		for _, o := range strings.Split(f[5], ".") {
			if v, err := strconv.Atoi(o); err == nil {
				c.omit = append(c.omit, v)
			}
		}
	}
	return c
}

// listenTrack keeps Browser Bridge's listen track on while listening, and
// says what it listens to -- or what is wrong -- on the screen.
type listenTrack struct {
	pm *pmclient.Client
	st *listenState

	mu   sync.Mutex
	on   bool
	last string
}

func (t *listenTrack) set(on bool) {
	t.mu.Lock()
	t.on = on
	t.mu.Unlock()
	if on {
		t.st.clear()
		t.poll()
		return
	}
	if _, err := t.pm.LiveListen(false, ""); err != nil {
		log.Printf("listen: off: %v", err)
	}
	t.st.clear()
}

func (t *listenTrack) poll() {
	t.mu.Lock()
	on := t.on
	t.mu.Unlock()
	if !on {
		return
	}
	st, err := t.pm.LiveListen(true, portListen)
	source, status := "", ""
	switch {
	case err != nil:
		status = "CANNOT REACH PUSH MANAGER"
	case !st.OK && strings.Contains(st.Error, "7704"):
		status = "ACTIVATE BROWSER BRIDGE 0.4 IN LIVE (SETTINGS > MIDI > CONTROL SCRIPTS)"
	case !st.OK && strings.Contains(st.Error, "no MIDI output"):
		status = "IN PUSH'S MIDI SETTINGS, SWITCH TRACK ON FOR STACKS LISTEN"
	case !st.OK:
		status = "LISTEN TRACK: " + strings.ToUpper(st.Error)
	default:
		source = st.Source
		if source == "" {
			status = "SELECT A MIDI TRACK TO LISTEN TO"
		}
	}
	t.st.setTrack(source, status)
	if state := source + "|" + status; state != t.last {
		t.last = state
		if err != nil {
			log.Printf("listen: %v", err)
		} else {
			log.Printf("listen: source %q, status %q, input %q, output %q (%s)", source, status, st.Input, st.Output, st.Error)
		}
	}
}

func (t *listenTrack) run(done <-chan struct{}) {
	tk := time.NewTicker(listenEvery)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return
		case <-tk.C:
			t.poll()
		}
	}
}
