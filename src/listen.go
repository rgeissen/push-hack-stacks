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
	l.mu.Unlock()
}

func (l *listenState) setTrack(source, status string) {
	l.mu.Lock()
	l.source, l.status = source, status
	l.mu.Unlock()
}

// scene builds a frame: the chord named by the core (chord_of), once per
// change of the held notes. A chord lifted note by note keeps its name for
// a moment -- the last notes released fade with it -- instead of flipping
// through every shrinking subset.
func (l *listenState) scene(eng *Engine, now time.Time, key, mask int) surface.ListenScene {
	notes, held, source, status := l.snapshot(now)
	if held != l.named {
		l.named = held
		var c chordOf
		if held != "" {
			var reply string
			eng.Call(func(i *dsp.Instance) { reply, _ = i.GetParam("chord_of:" + held) }, 200*time.Millisecond)
			c = parseChordOf(reply)
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
		}
		if c.name != l.name || c.inv != l.inv || c.bass != l.bass || c.interval != l.interval {
			if c.name != "" || len(notes) == 0 { // keep the name while the last notes fade
				prev := surface.BoardText(l.name, l.root, l.bass, l.known)
				if l.interval != "" {
					prev = l.name
				}
				l.prev, l.name, l.nameAt = prev, c.name, now
				l.root, l.bass, l.inv, l.known = c.root, c.bass, c.inv, c.kind == 1
				l.interval, l.omit = c.interval, c.omit
			}
		}
	}
	l.keysLo = surface.KeyWindow(l.keysLo, notes)
	return surface.ListenScene{Now: now, Notes: notes, Name: l.name, PrevName: l.prev, NameAt: l.nameAt,
		Root: l.root, Bass: l.bass, Inv: l.inv, Known: l.known, Interval: l.interval, Omit: l.omit,
		Key: key, Mask: mask, KeysLo: l.keysLo, Source: source, Status: status}
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
