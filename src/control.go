package main

// control.go -- the Push as Stacks' control surface.
//
// Stacks reads the Push's own port (16:0, found by name), which the ALSA
// sequencer broadcasts: push-manager keeps reading it too. Everything a
// control changes goes through Engine.Call: the C core has one caller,
// always.
//
// With Push Hack Home (push-manager's switcher, Shift + Help) Stacks is an
// app: push-manager lends it the surface -- the display, the MIDI intercept
// (Live keeps Play, Record, tempo and volume) and the LEDs -- when the user
// picks STACKS, and the session says focus and blur. The LIVE tab gives the
// surface back; Shift + LIVE switches to listen mode (the screen only).
// With an older push-manager Stacks takes the surface itself, and Shift +
// Help cycles Live -> edit -> listen as in 0.2.

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
	"github.com/rgeissen/push-hack-stacks/dsp"
	"github.com/rgeissen/push-hack-stacks/surface"
)

// passToLive are the controls Live keeps while Stacks owns the surface:
// Play (which also stops) and Record -- the two Move lets through -- and
// the ones you reach for while composing: Metronome, Tap Tempo, the tempo
// knob (turn and press) and the master volume. All are CC 0-87, so they fit
// push-display's pass bitmap.
var passToLive = []int{push3.CCPlay, push3.CCRecord, push3.CCMetronome, push3.CCTapTempo,
	push3.CCTempo, push3.CCTempoPress, push3.CCVolume}

const (
	holdToCopy   = 500 * time.Millisecond // a pattern pad held this long copies
	trigCooldown = 600 * time.Millisecond // one trigger per encoder gesture
)

type surfaceCtl struct {
	deps   *deps        // what taking the surface needs of the framework
	follow *follower    // Key and Scale following Push's scale; nil until main sets it
	listen *listenTrack // the listen track in Live; nil until main sets it
	eng    *Engine
	scr    *screen
	man    *surface.Manifest
	pm     *pmclient.Client

	// managed: push-manager has Push Hack Home, and lends Stacks the
	// surface; focused: it has lent it now.
	managed, focused atomic.Bool

	mu        sync.Mutex
	c         *alsaseq.Client // nil until MIDI is up
	push      alsaseq.Addr
	active    bool
	listening bool // listen mode: the screen only, pads and buttons with Live

	// Control goroutine only.
	shift, del, delUsed bool
	help, pendingTake   bool // Shift + Help seen; take once both are up
	acc                 [8]int
	lastTrig            [8]time.Time
	padAt               map[uint8]time.Time

	// Screen goroutine only (paint).
	leds     map[uint16]uint8 // kind<<8|number -> value last sent
	ledsFull time.Time        // when every LED was last re-sent

	ledMu sync.Mutex
	shown map[uint16]uint8 // what each LED last got, for GET /api/leds
}

// ledRefresh re-sends every LED this often. Push3 keeps writing LEDs while
// Stacks owns the surface -- Live lights the pads for the notes it plays --
// and only a re-send puts Stacks' colours back. The real fix is the hook
// holding Push3's LED output while intercept is on (plan item 14).
const ledRefresh = time.Second

func newSurface(eng *Engine, scr *screen, man *surface.Manifest) *surfaceCtl {
	pm := pmClient()
	s := &surfaceCtl{eng: eng, scr: scr, man: man, pm: pm, deps: newDeps(pm),
		padAt: map[uint8]time.Time{}}
	scr.pads = s.paint
	scr.managed = s.managed.Load
	scr.retake = func() {
		if s.mode() == modeEdit {
			if err := s.pm.SetMidiFilterPass(true, passToLive); err != nil {
				log.Printf("surface: intercept again: %v", err)
			}
			s.scr.Update(func(*look) {}) // and every LED with it
		}
	}
	return s
}

// attach connects the surface to the Push once /dev/snd may be opened.
func (s *surfaceCtl) attach() error {
	port, ok := alsaseq.FindByName("Ableton Push 3 Live Port", alsaseq.CapRead)
	if !ok {
		log.Print("surface: Push 3 Live Port not found; controls stay off")
		return nil
	}
	c, err := openPort("Push Hack Stacks Surface", "Stacks Surface", alsaseq.CapWrite|alsaseq.CapSubsWrite)
	if err != nil {
		return err
	}
	if err := c.Subscribe(port.Addr); err != nil {
		c.Close()
		return err
	}
	s.mu.Lock()
	s.c, s.push = c, port.Addr
	s.mu.Unlock()
	events := make(chan []byte, 256)
	go func() {
		err := c.ReadLoop(pushHandler{out: events})
		log.Printf("surface: %v", err)
		close(events)
	}()
	go func() {
		for msg := range events {
			s.handle(msg)
		}
	}()
	log.Printf("surface: reading the Push at %d:%d", port.Addr.Client, port.Addr.Port)
	return nil
}

// The surface's three modes, cycled by Shift + Help: Live has everything;
// Stacks owns the screen, pads and buttons (edit); Stacks has only the
// screen and names the chord being played (listen).
const (
	modeLive = iota
	modeEdit
	modeListen
)

var modeNames = []string{"live", "edit", "listen"}

// SetActive takes the surface for editing, or gives everything back.
func (s *surfaceCtl) SetActive(on bool, tab int) error {
	return s.setMode(map[bool]int{true: modeEdit, false: modeLive}[on], tab)
}

func (s *surfaceCtl) mode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.active:
		return modeEdit
	case s.listening:
		return modeListen
	}
	return modeLive
}

func (s *surfaceCtl) setMode(m, tab int) error {
	if s.managed.Load() {
		return s.askMode(m, tab)
	}
	cur := s.mode()
	if m != modeLive {
		if p := s.deps.Problem(); p != "" { // say what is missing, do not half-take
			s.deps.note(p)
			go s.scr.Notice(p) // on the screen too, when the screen works at all
			return errors.New(p)
		}
	}
	if m == modeEdit {
		if err := s.pm.SetMidiFilterPass(true, passToLive); err != nil {
			return err
		}
	}
	s.scr.SetListen(m == modeListen)
	if err := s.scr.Set(m != modeLive, tab); err != nil {
		return err
	}
	if cur == modeEdit && m != modeEdit {
		// The pads and buttons go back to Live; push-manager repaints
		// Live's LEDs when the filter goes off.
		if err := s.pm.SetMidiFilter(false); err != nil {
			return err
		}
	}
	s.apply(m, cur)
	return nil
}

// askMode is setMode under Push Hack Home: push-manager switches the
// display and the intercept. Editing from Live asks push-manager to open
// Stacks (the focus that answers applies it); listen and back to editing
// switch the intercept of a Stacks that already has the surface; Live
// gives it back (the blur applies it).
func (s *surfaceCtl) askMode(m, tab int) error {
	if tab >= 0 && tab < 7 {
		s.scr.Update(func(l *look) { l.tab = tab })
	}
	if m != modeLive {
		if p := s.deps.Problem(); p != "" {
			s.deps.note(p)
			return errors.New(p)
		}
	}
	cur := s.mode()
	switch {
	case m == modeLive:
		if !s.focused.Load() {
			return nil
		}
		return s.pm.SurfaceLeave("live")
	case !s.focused.Load():
		if err := s.pm.SurfaceOpen(); err != nil {
			return err
		}
		if m == modeListen {
			return s.pm.SurfaceInput(false) // opened, then the screen only
		}
		return nil
	case m == cur:
		return nil
	}
	if err := s.pm.SurfaceInput(m == modeEdit); err != nil {
		return err
	}
	s.apply(m, cur)
	return nil
}

// apply puts Stacks itself in mode m, coming from cur: the screen, the
// pads, parking, the listen track.
func (s *surfaceCtl) apply(m, cur int) {
	if s.managed.Load() {
		s.scr.SetListen(m == modeListen)
		s.scr.Set(m != modeLive, -1) //nolint:errcheck // managed: no call to push-manager
	}
	s.mu.Lock()
	s.active, s.listening = m == modeEdit, m == modeListen
	s.mu.Unlock()
	// Anything but editing, Stacks is PARKED: the DSP's own rule from Move
	// (Back parks the tool) -- Run goes off, so it stops playing along with
	// Live's transport on whatever track is selected, unless a stamp is
	// armed or under way; opening gives Run back. Reported on device:
	// "whenever Stacks is closed it still plays the Stacks chords when I
	// hit play". Listening is Live playing, not Stacks.
	s.set("parked", map[bool]string{true: "off", false: "on"}[m == modeEdit])
	if m == modeEdit {
		s.scr.Update(func(*look) {}) // force a frame, and with it a full LED paint
	}
	if s.listen != nil && (m == modeListen) != (cur == modeListen) {
		go s.listen.set(m == modeListen) // the listen track in Live, on or off
	}
	log.Printf("surface: %s", []string{"given back to Live", "editing (pads, buttons and screen)", "listening (pads and buttons with Live)"}[m])
}

// session holds Stacks' session with push-manager while Stacks runs: with
// Push Hack Home, focus and blur decide when Stacks has the surface.
// Without it (an older push-manager), Stacks takes the surface itself,
// and asks again later in case push-manager was updated.
func (s *surfaceCtl) session(done <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-done; cancel() }()
	for ctx.Err() == nil {
		if !s.pm.HasSwitcher() {
			s.managed.Store(false)
			select {
			case <-ctx.Done():
			case <-time.After(depsEvery):
			}
			continue
		}
		if !s.managed.Swap(true) {
			log.Printf("surface: push-manager has Push Hack Home: Shift + Help opens it, Stacks is there")
		}
		s.pm.RunSurface(ctx, passToLive, pmclient.SurfaceHandler{ //nolint:errcheck
			OnFocus: func() {
				s.focused.Store(true)
				s.apply(modeEdit, s.mode())
			},
			OnBlur: func() {
				s.focused.Store(false)
				s.apply(modeLive, s.mode())
			},
		})
	}
}

// statusLoop keeps Stacks' card in Home current: the key, and the
// progression with its length (each line about 14 characters on the card).
func (s *surfaceCtl) statusLoop(done <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var last [2]string
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if !s.managed.Load() {
			continue
		}
		var prog, key, scale, steps string
		s.eng.Call(func(i *dsp.Instance) {
			prog, _ = i.GetParam("progression")
			key, _ = i.GetParam("key")
			scale, _ = i.GetParam("scale")
			steps, _ = i.GetParam("steps")
		}, 500*time.Millisecond)
		second := steps + " CHORDS"
		if p := strings.TrimSpace(prog); p != "" && !strings.EqualFold(p, "none") {
			second = p + ", " + steps
		}
		lines := [2]string{strings.ToUpper(key + " " + scale), strings.ToUpper(second)}
		if lines == last {
			continue
		}
		if err := s.pm.SurfaceStatus(lines[0], lines[1]); err == nil {
			last = lines
		}
	}
}

// LEDs is the last value sent to each pad (row 0 at the bottom) and
// button (by CC), for tests: what the hardware shows, not what it should.
func (s *surfaceCtl) LEDs() map[string]any {
	s.ledMu.Lock()
	defer s.ledMu.Unlock()
	var pads [8][8]int
	ccs := map[string]int{}
	for k, v := range s.shown {
		n := int(k & 0xFF)
		if k&(1<<8) == 0 {
			ccs[strconv.Itoa(n)] = int(v)
		} else if n >= push3.PadNoteMin && n <= push3.PadNoteMax {
			pads[(n-push3.PadNoteMin)/8][(n-push3.PadNoteMin)%8] = int(v)
		}
	}
	return map[string]any{"active": s.isActive(), "pads": pads, "buttons": ccs}
}

// View is what the screen and pads are set to show, for tests: the tab,
// the editor page, the held legend, an armed slot action, the toast.
func (s *surfaceCtl) View() map[string]any {
	l := s.scr.Look()
	return map[string]any{"page": surface.PageNames[l.page], "legend": l.legend, "pat_mode": l.patMode,
		"toast": l.toast, "held": l.held, "scroll": l.scroll, "touched": l.touched}
}

func (s *surfaceCtl) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Close gives the surface back, for shutdown. Under Push Hack Home the
// session ends with the process and push-manager takes the surface back.
func (s *surfaceCtl) Close() {
	if s.isActive() && !s.managed.Load() {
		s.pm.SetMidiFilter(false)
	}
	if s.mode() == modeListen && s.listen != nil {
		s.listen.set(false) // the listen track's monitoring off: nothing flows
	}
	s.mu.Lock()
	if s.c != nil {
		s.c.Close()
	}
	s.mu.Unlock()
}

// ---- input --------------------------------------------------------------

type pushHandler struct{ out chan<- []byte }

func (h pushHandler) Fixed(evType uint8, _ alsaseq.Addr, data []byte) {
	if evType == alsaseq.EvSensing {
		return
	}
	if msg := alsaseq.DecodeMIDI(evType, data); msg != nil {
		h.out <- msg
	}
}

func (pushHandler) VarLen(uint8, alsaseq.Addr, []byte) {}

func (s *surfaceCtl) handle(msg []byte) {
	if len(msg) < 3 {
		return
	}
	kind, num, val := msg[0]&0xF0, msg[1], msg[2]
	// Push's buttons and encoders are on channel 1. With MPE the pads add
	// per-note expression on the note's own channel (2-16) -- and MPE's
	// slide is CC 74, the same number as encoder 4: every pad press turned
	// CHORD SHAPE (seen in the surface log as shape m13 -> dim -> rest on
	// each press). Controls are read on channel 1 only; pad notes on any.
	if kind == 0xB0 && msg[0]&0x0F != 0 {
		return
	}
	if kind == 0xB0 && num == push3.CCShift {
		s.shift = val > 0
	}
	if kind == 0xB0 && num == push3.CCHelp {
		s.help = val > 0
	}
	// Shift + Help cycles Live -> edit -> listen -> Live, on the RELEASE,
	// not the press: intercept switched on mid-chord hides the releases from
	// Push3, which then believes Shift and Help are still held -- and Play
	// became Shift + Play (seen on device: the button lit, nothing ran).
	cur := s.mode()
	if s.managed.Load() {
		// Shift + Help is push-manager's now (Push Hack Home), whatever mode.
		s.pendingTake = false
	} else if kind == 0xB0 && num == push3.CCHelp && val > 0 && s.shift {
		s.pendingTake = true
	}
	if s.pendingTake && !s.shift && !s.help {
		s.pendingTake = false
		next := (cur + 1) % 3
		if err := s.setMode(next, -1); err != nil {
			log.Printf("surface: mode %d: %v", next, err)
		}
		return
	}
	if cur != modeEdit { // Live has the controls; Stacks only watches for Shift + Help
		return
	}
	if kind == 0xB0 && num == push3.CCShift {
		return
	}
	if kind == 0x90 && val > 0 && push3.IsPadNote(num) || kind == 0xB0 && val > 0 && !push3.IsEncoderCC(num) {
		log.Printf("surface: %s %d", map[bool]string{true: "pad", false: "button"}[kind == 0x90], num)
	}
	switch kind {
	case 0xB0:
		s.cc(num, val)
	case 0x90, 0x80:
		if kind == 0x80 {
			val = 0
		}
		s.note(num, val)
	}
}

func (s *surfaceCtl) cc(cc, val uint8) {
	pressed := val > 0
	l := s.scr.Look()
	switch {
	case cc >= push3.CCScreenTop1 && cc <= push3.CCScreenTop8:
		i := int(cc - push3.CCScreenTop1)
		if i == 7 {
			// Give the surface back on the RELEASE: intercept is still on
			// for the press, so Push3 sees neither half of it. Under Push
			// Hack Home, Shift + LIVE is listen mode: the screen stays.
			if !pressed {
				m := modeLive
				if s.shift && s.managed.Load() {
					m = modeListen
				}
				if err := s.setMode(m, -1); err != nil {
					log.Printf("surface: release: %v", err)
				}
			}
			return
		}
		if pressed {
			s.scr.Update(func(l *look) { l.tab, l.patMode = i, "" })
		}

	case cc >= push3.CCScreenBot1 && cc <= push3.CCScreenBot8:
		if !pressed {
			return
		}
		acts := surface.Actions(l.tab)
		i := int(cc - push3.CCScreenBot1)
		switch {
		case i >= len(acts):
		case acts[i].Mode != "": // arm (or disarm) it for the next slot tapped
			m := acts[i].Mode
			s.scr.Update(func(l *look) { l.patMode = map[bool]string{true: "", false: m}[l.patMode == m] })
		case acts[i].Key != "":
			s.fire(acts[i].Key, acts[i].Value, acts[i].Label)
		}

	case push3.IsEncoderCC(cc) && cc >= push3.CCEncoder1 && cc <= push3.CCEncoder8:
		s.turn(int(cc-push3.CCEncoder1), push3.DecodeRel(val), l.tab)

	case cc == push3.CCJogWheel:
		d := push3.DecodeRel(val)
		if s.shift { // Shift + jog changes bank, as on Move
			s.scr.Update(func(l *look) { l.tab = ((l.tab+sign(d))%7 + 7) % 7 })
			return
		}
		s.step("sel", sign(d))

	case cc == push3.CCJogPress:
		s.set("play", map[bool]string{true: "on", false: "off"}[pressed])

	case cc >= push3.CCScene14 && cc <= push3.CCScene18:
		// Picks the page, and while held the screen shows the pad map:
		// what each row of that page does, before a finger lands on it.
		pg := surface.EditPage(cc - push3.CCScene14)
		s.scr.Update(func(l *look) {
			switch {
			case pressed:
				l.page, l.scroll, l.legend = pg, -1, true
			case l.page == pg:
				l.legend = false
			}
		})

	case cc == push3.CCDPadUp || cc == push3.CCDPadDown: // editor page, as Up/Down on Move
		if pressed {
			d := map[bool]int{true: -1, false: 1}[cc == push3.CCDPadUp]
			s.scr.Update(func(l *look) {
				l.page = surface.EditPage(clamp(int(l.page)+d, 0, len(surface.PageNames)-1))
				l.scroll = -1
			})
		}

	case cc == push3.CCDPadLeft || cc == push3.CCDPadRight: // chord, as the jog
		if pressed {
			s.step("sel", map[bool]int{true: -1, false: 1}[cc == push3.CCDPadLeft])
		}

	case cc == push3.CCPageLeft || cc == push3.CCPageRight: // scroll the page's long row
		if pressed {
			params, _ := s.eng.Snapshot()
			sc := surface.Scene{M: s.man, Params: params, Tab: l.tab, Held: -1, Scroll: l.scroll}
			if _, start, total, ok := surface.LongRow(sc, l.page); ok {
				d := map[bool]int{true: -8, false: 8}[cc == push3.CCPageLeft]
				next := clamp(start+d, 0, total-8)
				s.scr.Update(func(l *look) { l.scroll = next })
			}
		}

	case cc == push3.CCScale: // Key and Scale live on MAIN; Shift + Scale follows Push's
		if !pressed {
			return
		}
		if s.shift && s.follow != nil {
			s.follow.Set(!s.follow.On(), "")
		}
		s.scr.Update(func(l *look) { l.tab = 0 })

	case cc == push3.CCDelete:
		if pressed {
			s.del, s.delUsed = true, false
			return
		}
		s.del = false
		if !s.delUsed { // a plain press, not a modifier for a pattern pad
			s.fire("remove", "on", "DELETE")
		}

	case !pressed:
		return
	case cc == push3.CCAdd:
		s.fire("insert", "on", "INSERT")
	case cc == push3.CCDuplicate:
		s.fire("duplicate", "on", "DUPLICATE")
	case cc == push3.CCDoubleLoop:
		if s.shift {
			s.fire("compress", "on", "DOUBLE TIME")
		} else {
			s.fire("stretch", "on", "HALFTIME")
		}
	case cc == push3.CCMute:
		s.fire("cmute", "toggle", "MUTE")
	case cc == push3.CCUndo:
		if s.shift {
			s.fire("redo", "on", "REDO")
		} else {
			s.fire("undo", "on", "UNDO")
		}
	case cc == push3.CCNew:
		s.fire("clear", "on", "CLEARED")
	case cc == push3.CCCapture:
		s.fire("stamp", "on", "")
	case cc == push3.CCOctaveUp:
		s.step("octave", 1)
	case cc == push3.CCOctaveDown:
		s.step("octave", -1)
	}
}

func (s *surfaceCtl) note(n, vel uint8) {
	pressed := vel > 0
	if n <= push3.NoteEncoder8Touch {
		i := int(n)
		s.scr.Update(func(l *look) {
			switch {
			case pressed:
				l.touched = i
			case l.touched == i:
				l.touched = -1
			}
		})
		if !pressed {
			s.acc[i] = 0
		}
		return
	}
	if !push3.IsPadNote(n) {
		return
	}
	row, col := int(n-push3.PadNoteMin)/8, int(n-push3.PadNoteMin)%8
	switch {
	case row <= 1: // the chords: select, and hold to hear THAT chord
		i := row*8 + col
		if i >= s.count() {
			return
		}
		// Held, a pad loops ITS chord with its rhythm (stacks.c `audition`).
		// A note into Stacks (held_pad) sustains one strike with no figure,
		// and `play` runs the progression onward, so the chord heard drifts
		// off the pad held -- both tried here, both wrong for a chord pad.
		// A second pad pressed over the first takes over; releasing the
		// first then changes nothing.
		if pressed {
			s.set("sel", strconv.Itoa(i+1))
			s.set("audition", "on")
			s.scr.Update(func(l *look) { l.held = i })
			return
		}
		if s.scr.Look().held == i {
			s.set("audition", "off")
			s.scr.Update(func(l *look) { l.held = -1 })
		}
	case row <= 5: // the editor rows: write the selected chord
		if !pressed {
			return
		}
		l := s.scr.Look()
		params, _ := s.eng.Snapshot()
		g := surface.Pads(surface.Scene{M: s.man, Params: params, Tab: l.tab, Held: -1, Scroll: l.scroll}, l.page)
		if p := g[row][col]; p.Key != "" && p.Value != "" {
			s.set(p.Key, p.Value)
		}
	default: // the patterns: tap goes there, hold copies into it
		slot := strconv.Itoa((row-6)*8 + col + 1)
		if pressed {
			if s.del {
				s.delUsed = true
				s.fire("pat_delete", slot, "CLEARED SLOT "+slot)
				return
			}
			switch mode := s.scr.Look().patMode; mode { // armed on the bottom row
			case "clear", "copy":
				s.scr.Update(func(l *look) { l.patMode = "" })
				if mode == "clear" {
					s.fire("pat_delete", slot, "CLEARED SLOT "+slot)
				} else {
					s.fire("pat_store", slot, "COPIED TO SLOT "+slot)
				}
				return
			}
			s.padAt[n] = time.Now()
			return
		}
		at, ok := s.padAt[n]
		delete(s.padAt, n)
		if !ok {
			return
		}
		if time.Since(at) >= holdToCopy {
			s.fire("pat_store", slot, "COPIED TO SLOT "+slot)
		} else {
			s.set("pat_recall", slot)
		}
	}
}

// turn moves the bank's parameter under encoder i. Push's encoders send
// many small, accelerated ticks; a short list needs several per step or
// it skips past what you wanted, a long range one.
func (s *surfaceCtl) turn(i, delta, tab int) {
	if tab >= len(s.man.Banks) || i >= 8 {
		return
	}
	key := s.man.Banks[tab].Knobs[i]
	p, ok := s.man.Params[key]
	if key == "" || !ok || unavailable(key) {
		return
	}
	if (key == "key" || key == "scale") && s.follow != nil && s.follow.On() {
		// Your own turn wins: following would undo it within a second.
		s.follow.Set(false, ": YOU TURNED "+p.Label())
	}
	if p.Trigger { // fires once per gesture, either direction
		if time.Since(s.lastTrig[i]) > trigCooldown {
			s.fire(key, "on", p.Label())
		}
		s.lastTrig[i] = time.Now()
		return
	}
	span := len(p.Options)
	if span == 0 {
		span = p.Max - p.Min + 1
	}
	if n := encoderSteps(&s.acc[i], delta, span); n != 0 {
		s.step(key, n)
	}
}

// encoderSteps turns encoder ticks into value steps: `ticks` of them per
// step, more for a short list, and on short and medium lists at most 2
// ticks counted per message, so Push's acceleration cannot skip values.
// Reported on device as too sensitive at 4/2/1 ticks per step with no cap;
// the long lists (142 progressions, 0-100 ranges) keep their acceleration
// so they can still be crossed. acc carries the remainder between calls.
func encoderSteps(acc *int, delta, span int) int {
	ticks, maxPer := 6, 2
	switch {
	case span > 40:
		ticks, maxPer = 2, 64
	case span > 12:
		ticks = 3
	}
	*acc += clamp(delta, -maxPer, maxPer)
	n := *acc / ticks
	*acc -= n * ticks
	return n
}

// step moves a parameter n places along its options or range, clamped.
func (s *surfaceCtl) step(key string, n int) {
	p, ok := s.man.Params[key]
	if !ok && key != "sel" {
		return
	}
	s.eng.Call(func(i *dsp.Instance) {
		cur, _ := i.GetParam(key)
		var next string
		switch {
		case key == "sel":
			count, _ := i.GetParam("steps")
			v, _ := strconv.Atoi(cur)
			next = strconv.Itoa(clamp(v+n, 1, max(atoiSafe(count), 1)))
		case len(p.Options) > 0:
			at := 0
			for j, o := range p.Options {
				if o == cur {
					at = j
				}
			}
			next = p.Options[clamp(at+n, 0, len(p.Options)-1)]
		default:
			v, _ := strconv.Atoi(cur)
			next = strconv.Itoa(clamp(v+n, p.Min, p.Max))
		}
		if next != cur {
			i.SetParam(key, next)
			log.Printf("surface: %s %s -> %s", key, cur, next)
		}
	}, 500*time.Millisecond)
}

// fire writes a param from a button: "toggle" flips an on/off one.
func (s *surfaceCtl) fire(key, val, toast string) {
	s.eng.Call(func(i *dsp.Instance) {
		if val == "toggle" {
			cur, _ := i.GetParam(key)
			val = map[bool]string{true: "off", false: "on"}[cur == "on"]
		}
		i.SetParam(key, val)
		log.Printf("surface: %s = %s", key, val)
	}, 500*time.Millisecond)
	if toast != "" {
		s.scr.Toast(toast)
	}
}

// set writes one param, and says so: every change the surface makes is in
// the log, so a value that "changed by itself" can be traced to its press.
func (s *surfaceCtl) set(key, val string) {
	s.eng.Call(func(i *dsp.Instance) { i.SetParam(key, val) }, 500*time.Millisecond)
	log.Printf("surface: %s = %s", key, val)
}

func (s *surfaceCtl) count() int {
	var n string
	s.eng.Call(func(i *dsp.Instance) { n, _ = i.GetParam("steps") }, 500*time.Millisecond)
	return atoiSafe(n)
}

// ---- LEDs ----------------------------------------------------------------

// paint lights the pads and every button for the frame being drawn. Runs
// on the screen goroutine; sends only what changed since the last frame.
//
// EVERY button gets a value: a button with a meaning in Stacks is lit, the
// rest are switched off -- left alone they kept whatever Live last showed,
// and a dead button that looks alive is a lie. The buttons Live keeps
// (passToLive) are not touched, so Live's own lights show there.
func (s *surfaceCtl) paint(sc surface.Scene, page surface.EditPage) {
	s.mu.Lock()
	c, push, active := s.c, s.push, s.active
	s.mu.Unlock()
	if c == nil || !active {
		s.leds = nil // repaint everything next time the surface is taken
		return
	}
	if s.leds == nil || time.Since(s.ledsFull) > ledRefresh {
		s.leds, s.ledsFull = map[uint16]uint8{}, time.Now()
	}
	idx := func(name string) uint8 { i, _ := push3.ColorByName(name); return i }
	amber, white := idx("amber"), idx("white_btn")
	const lit = 127 // white-LED buttons: the value is brightness

	g := surface.Pads(sc, page)
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			n := uint8(push3.PadNoteMin + row*8 + col)
			s.led(c, push, true, n, g[row][col].Color)
		}
	}

	want := map[uint8]uint8{}
	for _, cc := range push3.LEDButtons {
		want[cc] = 0
	}
	for _, cc := range passToLive {
		delete(want, uint8(cc))
	}
	for i := 0; i < 8; i++ { // tabs: the bank you are on is amber
		want[uint8(push3.CCScreenTop1+i)] = map[bool]uint8{true: amber, false: white}[i == sc.Tab]
	}
	acts := surface.Actions(sc.Tab)
	params, _ := s.eng.Snapshot()
	prog, _ := surface.ParseProg(params["prog"])
	for i, a := range acts {
		if a.Label == "" {
			continue
		}
		v := white
		if a.IsLit(surface.Scene{Params: params, PatMode: sc.PatMode}, prog) {
			v = amber
		}
		want[uint8(push3.CCScreenBot1+i)] = v
	}
	for i := range surface.PageNames { // scene buttons: the editor page
		want[uint8(push3.CCScene14+i)] = map[bool]uint8{true: amber, false: white}[surface.EditPage(i) == page]
	}
	for _, cc := range []uint8{push3.CCAdd, push3.CCDelete, push3.CCDuplicate, push3.CCDoubleLoop,
		push3.CCMute, push3.CCUndo, push3.CCNew, push3.CCCapture, push3.CCOctaveUp, push3.CCOctaveDown,
		push3.CCShift, push3.CCHelp, push3.CCScale} {
		want[cc] = lit
	}
	// Arrows light only where they can go.
	if page > 0 {
		want[push3.CCDPadUp] = lit
	}
	if int(page) < len(surface.PageNames)-1 {
		want[push3.CCDPadDown] = lit
	}
	if prog.Sel > 1 {
		want[push3.CCDPadLeft] = lit
	}
	if prog.Sel < len(prog.Chords) {
		want[push3.CCDPadRight] = lit
	}
	if _, start, total, ok := surface.LongRow(sc, page); ok {
		if start > 0 {
			want[push3.CCPageLeft] = lit
		}
		if start+8 < total {
			want[push3.CCPageRight] = lit
		}
	}
	for cc, v := range want {
		s.led(c, push, false, cc, v)
	}
}

func (s *surfaceCtl) led(c *alsaseq.Client, push alsaseq.Addr, pad bool, n, v uint8) {
	k := uint16(n)
	if pad {
		k |= 1 << 8
	}
	if last, ok := s.leds[k]; ok && last == v {
		return
	}
	s.leds[k] = v
	s.ledMu.Lock()
	if s.shown == nil {
		s.shown = map[uint16]uint8{}
	}
	s.shown[k] = v
	s.ledMu.Unlock()
	if pad {
		c.SendNote(push, 0, n, v)
	} else {
		c.SendCC(push, 0, n, int32(v))
	}
}

// pmClient is a push-manager client that names Stacks (hack.json's id).
// While Stacks owns the surface push-manager refuses (409) the frames and
// the intercept calls of a client that does not: the screen's own client
// did not, and under Push Hack Home Stacks' screen never showed -- Home's
// last frame stayed up while Stacks lit the pads (found 2026-10-17).
func pmClient() *pmclient.Client {
	c := pmclient.New("http://127.0.0.1:7701")
	c.App = "stacks"
	return c
}

func unavailable(key string) bool { return key == "read" || key == "read_mode" || key == "read_track" }

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }
