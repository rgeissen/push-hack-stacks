package main

// control.go -- the Push as Stacks' control surface.
//
// Stacks reads the Push's own port (16:0, found by name), which the ALSA
// sequencer broadcasts: push-manager keeps reading it too. Shift + Help
// takes the surface -- the display, MIDI intercept (so Live stops seeing
// pads and buttons, except Play and Record), and the LEDs -- and the LIVE
// tab gives it all back. Everything a control changes goes through
// Engine.Call: the C core has one caller, always.

import (
	"errors"
	"log"
	"strconv"
	"sync"
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
	deps *deps // what taking the surface needs of the framework
	eng  *Engine
	scr  *screen
	man  *surface.Manifest
	pm   *pmclient.Client

	mu     sync.Mutex
	c      *alsaseq.Client // nil until MIDI is up
	push   alsaseq.Addr
	active bool

	// Control goroutine only.
	shift, del, delUsed bool
	help, pendingTake   bool // Shift + Help seen; take once both are up
	pendingRelease      bool // Shift + Help while active; give back once both are up
	acc                 [8]int
	lastTrig            [8]time.Time
	padAt               map[uint8]time.Time

	// Screen goroutine only (paint).
	leds     map[uint16]uint8 // kind<<8|number -> value last sent
	ledsFull time.Time        // when every LED was last re-sent
}

// ledRefresh re-sends every LED this often. Push3 keeps writing LEDs while
// Stacks owns the surface -- Live lights the pads for the notes it plays --
// and only a re-send puts Stacks' colours back. The real fix is the hook
// holding Push3's LED output while intercept is on (plan item 14).
const ledRefresh = time.Second

func newSurface(eng *Engine, scr *screen, man *surface.Manifest) *surfaceCtl {
	pm := pmclient.New("http://127.0.0.1:7701")
	s := &surfaceCtl{eng: eng, scr: scr, man: man, pm: pm, deps: newDeps(pm),
		padAt: map[uint8]time.Time{}}
	scr.pads = s.paint
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

// SetActive takes the surface or gives it back.
func (s *surfaceCtl) SetActive(on bool, tab int) error {
	if on {
		if p := s.deps.Problem(); p != "" { // say what is missing, do not half-take
			s.deps.note(p)
			go s.scr.Notice(p) // on the screen too, when the screen works at all
			return errors.New(p)
		}
		if err := s.pm.SetMidiFilterPass(true, passToLive); err != nil {
			return err
		}
	}
	if err := s.scr.Set(on, tab); err != nil {
		return err
	}
	if !on {
		// push-manager repaints Live's LEDs when the filter goes off.
		if err := s.pm.SetMidiFilter(false); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.active = on
	s.mu.Unlock()
	// Closed, Stacks is PARKED: the DSP's own rule from Move (Back parks
	// the tool) -- Run goes off, so it stops playing along with Live's
	// transport on whatever track is selected, unless a stamp is armed or
	// under way; opening gives Run back. Reported on device: "whenever
	// Stacks is closed it still plays the Stacks chords when I hit play".
	s.set("parked", map[bool]string{true: "off", false: "on"}[on])
	if on {
		s.scr.Update(func(*look) {}) // force a frame, and with it a full LED paint
	}
	log.Printf("surface: %s", map[bool]string{true: "taken (Shift + Help)", false: "given back to Live"}[on])
	return nil
}

func (s *surfaceCtl) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Close gives the surface back, for shutdown.
func (s *surfaceCtl) Close() {
	if s.isActive() {
		s.pm.SetMidiFilter(false)
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
	active := s.isActive()
	if !active {
		// Take the surface on the RELEASE of Shift + Help, not the press:
		// intercept switched on mid-chord hides the releases from Push3,
		// which then believes Shift and Help are still held -- and Play
		// became Shift + Play (seen on device: the button lit, nothing ran).
		if kind == 0xB0 && num == push3.CCHelp && val > 0 && s.shift {
			s.pendingTake = true
		}
		if s.pendingTake && !s.shift && !s.help {
			s.pendingTake = false
			if err := s.SetActive(true, -1); err != nil {
				log.Printf("surface: take: %v", err)
			}
		}
		return
	}
	// Shift + Help closes as it opens, on the release.
	if kind == 0xB0 && num == push3.CCHelp && val > 0 && s.shift {
		s.pendingRelease = true
	}
	if s.pendingRelease && !s.shift && !s.help {
		s.pendingRelease = false
		if err := s.SetActive(false, -1); err != nil {
			log.Printf("surface: release: %v", err)
		}
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
			// for the press, so Push3 sees neither half of it.
			if !pressed {
				if err := s.SetActive(false, -1); err != nil {
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

	case cc == push3.CCScale: // Key and Scale live on MAIN
		if pressed {
			s.scr.Update(func(l *look) { l.tab = 0 })
		}

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
	th := 4
	switch {
	case span > 40:
		th = 1
	case span > 12:
		th = 2
	}
	s.acc[i] += delta
	n := s.acc[i] / th
	s.acc[i] -= n * th
	if n != 0 {
		s.step(key, n)
	}
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
	if pad {
		c.SendNote(push, 0, n, v)
	} else {
		c.SendCC(push, 0, n, int32(v))
	}
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
