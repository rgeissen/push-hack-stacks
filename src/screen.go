package main

// screen.go -- Stacks on Push's display.
//
// Frames go through push-manager's /api/display/frame as raw pixels
// (core/pmclient.PushFrame): push-manager is the framebuffer's only writer,
// and a second writer is what the single-writer rule exists to stop. A PNG
// per frame through /api/display/image cost ~11 ms here and pushed the
// block timer's worst lateness from ~2 ms to 5 ms.

import (
	"hash/fnv"
	"image"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/rgeissen/push-hack-stacks/dsp"
	"github.com/rgeissen/push-hack-stacks/surface"
)

const (
	frameEvery  = 66 * time.Millisecond // ~15 fps, and only on change
	checkEvery  = time.Second           // display mode and Sync link
	toastFor    = 1500 * time.Millisecond
	syncHint    = "LIVE IS NOT SENDING CLOCK: SETTINGS > MIDI > STACKS IN > SYNC ON"
	seqClients  = "/proc/asound/seq/clients"
	ownPortName = "\"Stacks In\""
)

// holdState is the held pad's audition: which chord, and when it began.
type holdState struct {
	chord int
	at    time.Time
}

// look is what the hardware selects and the screen and pads draw: shared
// by the control surface (which writes it) and the display (which reads it).
type look struct {
	tab     int
	page    surface.EditPage
	touched int
	held    int  // chord a pad is auditioning, -1 none
	scroll  int  // start of the page's long editor row, -1 follows the value
	legend  bool // a scene button is held: the screen shows the pad map
	toast   string
	toastAt time.Time
}

type screen struct {
	eng *Engine
	man *surface.Manifest
	pm  *pmclient.Client

	mu   sync.Mutex
	on   bool
	look look

	pads   func(surface.Scene, surface.EditPage) // LED painter, set by the surface
	last   uint64                                // hash of the last frame sent
	pos    int                                   // last whole-unit position, for interpolation
	posAt  time.Time
	hold   holdState // the held pad's audition, for its own playhead
	synced bool      // Live is connected to Stacks In
	stats  struct {
		sent      int
		total, mx time.Duration
		next      time.Time
	}
}

func newScreen(eng *Engine, man *surface.Manifest) *screen {
	return &screen{hold: holdState{chord: -1}, eng: eng, man: man, pm: pmclient.New("http://127.0.0.1:7701"),
		look: look{tab: 2, touched: -1, held: -1, scroll: -1}, synced: true}
}

// Set takes the display (mode 2) or gives it back to Live (mode 0).
func (s *screen) Set(on bool, tab int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tab >= 0 && tab < 7 {
		s.look.tab = tab
	}
	s.last = 0 // redraw on the next tick
	if on == s.on {
		return nil
	}
	if err := s.pm.SetMode(map[bool]int{true: 2, false: 0}[on]); err != nil {
		return err
	}
	s.on = on
	return nil
}

func (s *screen) State() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on, s.look.tab
}

// Look returns a copy of the shared selection; Update changes it.
func (s *screen) Look() look {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.look
}

func (s *screen) Update(f func(*look)) {
	s.mu.Lock()
	f(&s.look)
	s.last = 0
	s.mu.Unlock()
}

// Toast shows a short answer to something just done.
func (s *screen) Toast(msg string) {
	s.Update(func(l *look) { l.toast, l.toastAt = msg, time.Now() })
}

// run draws until done, then hands the display back.
func (s *screen) run(done <-chan struct{}) {
	t := time.NewTicker(frameEvery)
	defer t.Stop()
	img := image.NewNRGBA(image.Rect(0, 0, surface.W, surface.H))
	var nextCheck time.Time
	for {
		select {
		case <-done:
			if on, _ := s.State(); on {
				s.pm.SetMode(0)
			}
			return
		case now := <-t.C:
			if now.After(nextCheck) {
				nextCheck = now.Add(checkEvery)
				s.check()
			}
			if on, _ := s.State(); on {
				s.frame(img, now)
			}
		}
	}
}

// check re-takes the display after a Push3 restart (the hook starts in
// mode 0) and notices whether Live is sending clock to Stacks In at all:
// Live forgets that port's Sync whenever it is stopped uncleanly.
func (s *screen) check() {
	b, err := os.ReadFile(seqClients)
	synced := err != nil || portConnected(string(b), ownPortName)
	s.mu.Lock()
	if synced != s.synced {
		s.last = 0
	}
	s.synced = synced
	on := s.on
	s.mu.Unlock()
	if !on {
		return
	}
	if st, err := s.pm.DisplayStatus(); err == nil && st.Connected && st.Mode != 2 {
		log.Printf("screen: display was reset to mode %d (Push3 restart?), taking it again", st.Mode)
		if s.pm.SetMode(2) == nil {
			s.mu.Lock()
			s.last = 0
			s.mu.Unlock()
		}
	}
}

// portConnected reports whether a port in /proc/asound/seq/clients has
// anything connected to it.
func portConnected(clients, port string) bool {
	i := strings.Index(clients, port)
	if i < 0 {
		return false
	}
	rest := clients[i:]
	if j := strings.Index(rest[1:], "\n  Port "); j >= 0 {
		rest = rest[:j+1]
	}
	if j := strings.Index(rest, "\nClient "); j >= 0 {
		rest = rest[:j]
	}
	return strings.Contains(rest, "Connected From")
}

func (s *screen) frame(img *image.NRGBA, now time.Time) {
	params, ready := s.eng.Snapshot()
	if !ready {
		return
	}
	// The position moves every block: read it fresh, with the prog that
	// says which chord is sounding. Both are in-process calls.
	var rui, prog string
	if !s.eng.Call(func(i *dsp.Instance) {
		rui, _ = i.GetParam("rui_poll")
		prog, _ = i.GetParam("prog")
	}, 200*time.Millisecond) {
		return
	}
	params["prog"] = prog
	s.mu.Lock()
	l, synced := s.look, s.synced
	s.mu.Unlock()
	head := s.head(rui, prog, now)
	if l.held >= 0 {
		head = s.heldHead(l.held, rui, prog, now)
	} else {
		s.hold.chord = -1
	}

	s.mu.Lock()
	if l.toast != "" && now.Sub(l.toastAt) > toastFor {
		s.look.toast, l.toast = "", ""
	}
	s.mu.Unlock()

	toast := l.toast
	switch {
	case params["status"] == "armed":
		toast = "ARMED - PRESS RECORD"
	case toast == "" && !synced && params["run"] == "on":
		toast = syncHint
	}
	sc := surface.Scene{M: s.man, Params: params, Tab: l.tab, Touched: l.touched, Head: head,
		Blink: now.UnixMilli()/500%2 == 1, Toast: toast, Held: l.held, Scroll: l.scroll,
		Legend: l.legend, Page: l.page}
	if s.pads != nil {
		s.pads(sc, l.page)
	}
	surface.Render(img, sc)
	h := fnv.New64a()
	h.Write(img.Pix)
	sum := h.Sum64()
	if sum == s.last {
		return // nothing moved: send nothing
	}
	start := time.Now()
	if err := s.pm.PushFrame(img); err != nil {
		log.Printf("screen: %v", err)
		return
	}
	d := time.Since(start)
	s.last = sum
	st := &s.stats
	st.sent++
	st.total += d
	st.mx = max(st.mx, d)
	if now.After(st.next) {
		if st.sent > 0 {
			log.Printf("screen: %d frames, send avg %v max %v", st.sent,
				(st.total / time.Duration(st.sent)).Round(time.Microsecond), st.mx.Round(time.Microsecond))
		}
		st.sent, st.total, st.mx, st.next = 0, 0, 0, now.Add(time.Minute)
	}
}

// heldHead puts the playhead inside the chord a pad is holding: Stacks'
// clock runs during the audition (one unit per eighth of a bar, measured)
// but reports no transport, and its position is the global clock. So the
// head starts at the chord's onset when the pad goes down and sweeps its
// length, wrapping -- the chord's own figure repeating from the press.
func (s *screen) heldHead(chord int, rui, prog string, now time.Time) float64 {
	f := strings.Split(rui, ":") // rev:on:pos:bpm[:devms]
	p, ok := surface.ParseProg(prog)
	if len(f) < 4 || !ok || chord >= len(p.Chords) {
		return 0
	}
	bpm := atoiSafe(f[3])
	if s.hold.chord != chord {
		s.hold.chord, s.hold.at = chord, now
	}
	unitsPerSec := float64(max(bpm, 1)) / 60 * float64(p.StepUnits) / 4
	elapsed := now.Sub(s.hold.at).Seconds() * unitsPerSec
	c := p.Chords[chord]
	local := elapsed - float64(int(elapsed)/c.Len*c.Len)
	return float64(p.Starts()[chord]) + local
}

// head interpolates the playhead between whole-unit position reads, so it
// glides instead of jumping an eighth of a bar at a time.
func (s *screen) head(rui, prog string, now time.Time) float64 {
	f := strings.Split(rui, ":") // rev:on:pos:bpm[:devms]
	if len(f) < 4 || f[1] != "1" {
		s.posAt = time.Time{}
		return 0
	}
	pos, bpm := atoiSafe(f[2]), atoiSafe(f[3])
	if pos != s.pos || s.posAt.IsZero() {
		s.pos, s.posAt = pos, now
	}
	p, ok := surface.ParseProg(prog)
	if !ok || bpm <= 0 {
		return float64(pos)
	}
	unitsPerSec := float64(bpm) / 60 * float64(p.StepUnits) / 4
	return float64(pos) + min(now.Sub(s.posAt).Seconds()*unitsPerSec, 0.999)
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}
