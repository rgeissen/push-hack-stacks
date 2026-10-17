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

// passToLive are the buttons Live keeps while Stacks owns the surface:
// Play (which also stops) and Record -- the two Move lets through.
var passToLive = []int{push3.CCPlay, push3.CCRecord}

const (
	holdToCopy   = 500 * time.Millisecond // a pattern pad held this long copies
	trigCooldown = 600 * time.Millisecond // one trigger per encoder gesture
)

type surfaceCtl struct {
	eng *Engine
	scr *screen
	man *surface.Manifest
	pm  *pmclient.Client

	mu     sync.Mutex
	c      *alsaseq.Client // nil until MIDI is up
	push   alsaseq.Addr
	active bool

	// Control goroutine only.
	shift, del, delUsed bool
	help, pendingTake   bool // Shift + Help seen; take once both are up
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
	s := &surfaceCtl{eng: eng, scr: scr, man: man, pm: pmclient.New("http://127.0.0.1:7701"),
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
			s.scr.Update(func(l *look) { l.tab = i })
		}

	case cc >= push3.CCScreenBot1 && cc <= push3.CCScreenBot8:
		if !pressed {
			return
		}
		acts := surface.Actions(l.tab)
		if i := int(cc - push3.CCScreenBot1); i < len(acts) && acts[i].Key != "" {
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
		if pressed {
			s.scr.Update(func(l *look) { l.page = surface.EditPage(cc - push3.CCScene14) })
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
		// A note into Stacks selects a chord by pitch class and sounds only
		// that chord while it is held (stacks.c held_pad). `play` instead
		// plays the progression ONWARD from the chord, so the chord you hear
		// drifts away from the pad you hold (reported on device as "it
		// randomly changes chord"). Pitch classes cover chords 1-12; notes
		// 0-11 sit far below any chord voice, so the own-voice filter never
		// takes them for Stacks' own tail. Chords 13-16 keep play-onward.
		if i < 12 {
			if pressed {
				s.set("sel", strconv.Itoa(i+1))
			}
			s.scr.Update(func(l *look) {
				switch {
				case pressed:
					l.held = i
				case l.held == i:
					l.held = -1
				}
			})
			msg := []byte{0x90, byte(i), 100}
			if !pressed {
				msg = []byte{0x80, byte(i), 0}
			}
			s.eng.Call(func(in *dsp.Instance) { in.OnMIDI(msg, dsp.SourceExternal) }, 500*time.Millisecond)
			return
		}
		if pressed {
			s.set("sel", strconv.Itoa(i+1))
			s.set("play", "on")
			return
		}
		s.set("play", "off")
	case row <= 5: // the editor rows: write the selected chord
		if !pressed {
			return
		}
		l := s.scr.Look()
		params, _ := s.eng.Snapshot()
		g := surface.Pads(surface.Scene{M: s.man, Params: params, Tab: l.tab, Held: -1}, l.page)
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

// paint lights the pads and buttons for the frame being drawn. Runs on the
// screen goroutine; sends only what changed since the last frame.
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

	g := surface.Pads(sc, page)
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			n := uint8(push3.PadNoteMin + row*8 + col)
			s.led(c, push, true, n, g[row][col].Color)
		}
	}
	for i := 0; i < 8; i++ { // tabs: the bank you are on is amber
		v := white
		if i == sc.Tab {
			v = amber
		}
		s.led(c, push, false, uint8(push3.CCScreenTop1+i), v)
	}
	acts := surface.Actions(sc.Tab)
	params, _ := s.eng.Snapshot()
	prog, _ := surface.ParseProg(params["prog"])
	for i := 0; i < 8; i++ {
		v := uint8(0)
		if i < len(acts) && acts[i].Label != "" {
			v = white
			if acts[i].Lit != nil && acts[i].Lit(params, prog) {
				v = amber
			}
		}
		s.led(c, push, false, uint8(push3.CCScreenBot1+i), v)
	}
	for i := 0; i < 8; i++ { // scene buttons: the editor page
		v := uint8(0)
		if i < len(surface.PageNames) {
			v = white
			if surface.EditPage(i) == page {
				v = amber
			}
		}
		s.led(c, push, false, uint8(push3.CCScene14+i), v)
	}
	for _, cc := range []uint8{push3.CCAdd, push3.CCDelete, push3.CCDuplicate, push3.CCDoubleLoop,
		push3.CCMute, push3.CCUndo, push3.CCNew, push3.CCCapture, push3.CCOctaveUp, push3.CCOctaveDown} {
		s.led(c, push, false, cc, 127) // white buttons: the value is brightness
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
