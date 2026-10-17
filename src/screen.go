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
	frameEvery       = 66 * time.Millisecond // ~15 fps, and only on change
	listenFrameEvery = 33 * time.Millisecond // listen mode: ~30 fps while something moves
	checkEvery       = time.Second           // display mode and Sync link
	toastFor         = 1500 * time.Millisecond
	syncHint         = "LIVE IS NOT SENDING CLOCK: SETTINGS > MIDI > STACKS IN > SYNC ON"
	seqClients       = "/proc/asound/seq/clients"
	ownPortName      = "\"Stacks In\""
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
	held    int    // chord a pad is auditioning, -1 none
	scroll  int    // start of the page's long editor row, -1 follows the value
	legend  bool   // a scene button is held: the screen shows the pad map
	patMode string // PATTERNS: "clear"/"copy" armed for the next slot tapped
	toast   string
	toastAt time.Time
	undoAt  time.Time // an Undo or Redo was fired: the staff takes back its zoom
}

type screen struct {
	eng *Engine
	man *surface.Manifest
	pm  *pmclient.Client

	mu   sync.Mutex
	on   bool
	look look

	pads      func(surface.Scene, surface.EditPage) // LED painter, set by the surface
	last      uint64                                // hash of the last frame sent
	pos       int                                   // last whole-unit position, for interpolation
	posAt     time.Time
	hold      holdState         // the held pad's audition, for its own playhead
	lanes     surface.LaneRange // the staff's window, kept steady between frames
	zoom      surface.ZoomMemory
	follows   func() bool  // Key and Scale follow Push's; nil before main sets it
	listen    *listenState // what is played, in listen mode; nil before main sets it
	listening bool         // listen mode: draw the played chord, not the editor
	retake    func()       // re-takes what the mode holds besides the display; set by the surface
	managed   func() bool  // Push Hack Home lends the surface: push-manager switches the display
	synced    bool         // Live is connected to Stacks In
	stats     struct {
		sent      int
		total, mx time.Duration
		next      time.Time
		failed    int    // frames refused since the last that went through
		failErr   string // the failure last logged
	}
}

func newScreen(eng *Engine, man *surface.Manifest) *screen {
	return &screen{hold: holdState{chord: -1}, eng: eng, man: man, pm: pmClient(),
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
	// Under Push Hack Home push-manager has switched the display already.
	if s.managed == nil || !s.managed() {
		if err := s.pm.SetMode(map[bool]int{true: 2, false: 0}[on]); err != nil {
			return err
		}
	}
	s.on = on
	return nil
}

// SetListen switches the screen between the editor and the listen view.
func (s *screen) SetListen(on bool) {
	s.mu.Lock()
	s.listening, s.last = on, 0
	s.mu.Unlock()
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

// Notice shows a message on Push's screen for a few seconds and gives the
// screen back -- for the one case where Stacks cannot take the surface
// but the display works (an older Push Hack), so Shift + Help is not met
// with nothing. PushImage, not PushFrame: an older push-manager has no
// raw frame endpoint.
func (s *screen) Notice(msg string) {
	if on, _ := s.State(); on {
		return
	}
	if st, err := s.pm.DisplayStatus(); err != nil || !st.Connected {
		return
	}
	img := image.NewNRGBA(image.Rect(0, 0, surface.W, surface.H))
	surface.RenderNotice(img, "STACKS CANNOT OPEN ON PUSH", msg)
	if s.pm.SetMode(2) != nil {
		return
	}
	s.pm.PushImage(img)
	time.Sleep(4 * time.Second)
	if on, _ := s.State(); !on {
		s.pm.SetMode(0)
	}
}

// Undoing says an Undo or Redo is about to be fired, so the staff gives
// back the zoom of the state it returns to. Said before the fire: a frame
// that lands between the two then still knows.
func (s *screen) Undoing() {
	s.Update(func(l *look) { l.undoAt = time.Now() })
}

// Toast shows a short answer to something just done.
func (s *screen) Toast(msg string) {
	s.Update(func(l *look) { l.toast, l.toastAt = msg, time.Now() })
}

// run draws until done, then hands the display back.
func (s *screen) run(done <-chan struct{}) {
	t := time.NewTicker(listenFrameEvery)
	defer t.Stop()
	img := image.NewNRGBA(image.Rect(0, 0, surface.W, surface.H))
	var nextCheck, nextEdit time.Time
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
			on, _ := s.State()
			s.mu.Lock()
			listening := s.listening
			s.mu.Unlock()
			switch {
			case on && listening:
				s.listenFrame(img, now)
			case on && !now.Before(nextEdit): // the editor keeps its ~15 fps
				nextEdit = now.Add(frameEvery - listenFrameEvery/2)
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
	if !on || (s.managed != nil && s.managed()) { // Push Hack Home re-takes nothing: push-manager owns it
		return
	}
	if st, err := s.pm.DisplayStatus(); err == nil && st.Connected && st.Mode != 2 {
		log.Printf("screen: display was reset to mode %d (Push3 or push-manager restart?), taking it again", st.Mode)
		if err := s.pm.SetMode(2); err != nil {
			log.Printf("screen: %v", err) // "Push is shutting down": Push3 keeps it
			return
		}
		s.mu.Lock()
		s.last = 0
		retake := s.retake
		s.mu.Unlock()
		if retake != nil {
			retake() // the intercept went with the display: take both, or the pads go to Live under the editor
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
	var rui, prog, slots string
	if !s.eng.Call(func(i *dsp.Instance) {
		rui, _ = i.GetParam("rui_poll")
		prog, _ = i.GetParam("prog")
		slots, _ = i.GetParam("pat_names")
	}, 200*time.Millisecond) {
		return
	}
	params["prog"], params["pat_names"] = prog, slots
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
		Legend: l.legend, Page: l.page, PatMode: l.patMode}
	sc.FollowScale = s.follows != nil && s.follows()
	if pr, ok := surface.ParseProg(prog); ok {
		undo := !l.undoAt.IsZero() && now.Sub(l.undoAt) < time.Second
		var used bool
		if s.lanes, used = s.zoom.Fit(pr, params["lanes"], s.lanes, undo); used {
			s.mu.Lock()
			s.look.undoAt = time.Time{}
			s.mu.Unlock()
		}
		sc.Lanes = s.lanes
	}
	if s.pads != nil {
		s.pads(sc, l.page)
	}
	surface.Render(img, sc)
	s.send(img, now)
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

// send pushes a frame unless it is the one already on the screen, and
// keeps the send-time statistics.
func (s *screen) send(img *image.NRGBA, now time.Time) {
	h := fnv.New64a()
	h.Write(img.Pix)
	sum := h.Sum64()
	if sum == s.last {
		return // nothing moved: send nothing
	}
	start := time.Now()
	st := &s.stats
	if err := s.pm.PushFrame(img); err != nil {
		// Once per kind of failure, not per frame: a refused frame came back
		// ~15 times a second and wrote 30,000 log lines in a day.
		if msg := err.Error(); msg != st.failErr {
			log.Printf("screen: %v (said once; frames keep being tried)", err)
			st.failErr = msg
		}
		st.failed++
		return
	}
	if st.failed > 0 {
		log.Printf("screen: frames go through again, after %d failed", st.failed)
		st.failed, st.failErr = 0, ""
	}
	d := time.Since(start)
	s.last = sum
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

// listenFrame draws the chord being played (listen mode). Key and Scale
// come from the state snapshot, so a frame costs the core nothing; the
// core is asked only for a chord's name, when the held notes change.
func (s *screen) listenFrame(img *image.NRGBA, now time.Time) {
	if s.listen == nil {
		return
	}
	params, ready := s.eng.Snapshot()
	if !ready {
		return
	}
	key := 0
	for i, k := range keyNames {
		if k == params["key"] {
			key = i
		}
	}
	mask := 0
	for m, name := range stacksScales {
		if name == params["scale"] {
			mask = m
		}
	}
	sc := s.listen.scene(s.eng, now, key, mask)
	sc.KeyName = strings.ToUpper(params["key"] + " " + params["scale"])
	if s.managed != nil && s.managed() {
		// Shift + Help is push-manager's: it opens Home, where LIVE (or Shift +
		// Help again) gives Push back. "BACK TO LIVE" was the 0.2 cycle's.
		sc.Leave = "SHIFT + HELP: PUSH HACK HOME"
	}
	surface.RenderListen(img, sc)
	s.send(img, now)
}
