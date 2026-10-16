package main

// screen.go -- Stacks on Push's display.
//
// Frames go through push-manager's /api/display/* (core/pmclient): it is
// the framebuffer's only writer, and a second writer is what the
// single-writer rule exists to stop. Display only, for now: pads and
// encoders still reach Live. POST /api/screen turns it on or off and picks
// the bank until the top buttons are wired.

import (
	"hash/fnv"
	"image"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/rgeissen/push-hack-stacks/dsp"
	"github.com/rgeissen/push-hack-stacks/surface"
)

const frameEvery = 66 * time.Millisecond // ~15 fps, and only on change

type screen struct {
	eng *Engine
	man *surface.Manifest
	pm  *pmclient.Client

	mu  sync.Mutex
	on  bool
	tab int

	last     uint64 // hash of the last frame sent
	pos      int    // last whole-unit position read, for interpolation
	posAt    time.Time
	sent     int
	sendTime time.Duration
	maxSend  time.Duration
	nextLog  time.Time
}

func newScreen(eng *Engine, man *surface.Manifest) *screen {
	return &screen{eng: eng, man: man, pm: pmclient.New("http://127.0.0.1:7701"), tab: 2}
}

// Set takes the display (mode 2) or gives it back to Live (mode 0).
func (s *screen) Set(on bool, tab int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tab >= 0 && tab < 8 {
		s.tab = tab
	}
	s.last = 0 // redraw on the next tick
	if on == s.on {
		return nil
	}
	mode := 0
	if on {
		mode = 2
	}
	if err := s.pm.SetMode(mode); err != nil {
		return err
	}
	s.on = on
	return nil
}

func (s *screen) State() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on, s.tab
}

// run draws until done, then hands the display back.
func (s *screen) run(done <-chan struct{}) {
	t := time.NewTicker(frameEvery)
	defer t.Stop()
	img := image.NewNRGBA(image.Rect(0, 0, surface.W, surface.H))
	for {
		select {
		case <-done:
			if on, _ := s.State(); on {
				s.pm.SetMode(0)
			}
			return
		case now := <-t.C:
			on, tab := s.State()
			if !on {
				continue
			}
			s.frame(img, tab, now)
		}
	}
}

func (s *screen) frame(img *image.NRGBA, tab int, now time.Time) {
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
	head := s.head(rui, prog, now)

	surface.Render(img, surface.Scene{M: s.man, Params: params, Tab: tab, Touched: -1, Head: head,
		Blink: now.UnixMilli()/500%2 == 1, Toast: toastFor(params)})
	h := fnv.New64a()
	h.Write(img.Pix)
	sum := h.Sum64()
	if sum == s.last {
		return // nothing moved: send nothing
	}
	start := time.Now()
	if err := s.pm.PushImage(img); err != nil {
		log.Printf("screen: %v", err)
		return
	}
	d := time.Since(start)
	s.last = sum
	s.sent++
	s.sendTime += d
	s.maxSend = max(s.maxSend, d)
	if now.After(s.nextLog) {
		if s.sent > 0 {
			log.Printf("screen: %d frames, send avg %v max %v", s.sent,
				(s.sendTime / time.Duration(s.sent)).Round(time.Microsecond), s.maxSend.Round(time.Microsecond))
		}
		s.sent, s.sendTime, s.maxSend, s.nextLog = 0, 0, 0, now.Add(time.Minute)
	}
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
	frac := now.Sub(s.posAt).Seconds() * unitsPerSec
	return float64(pos) + min(frac, 0.999)
}

// toastFor is what the module's status asks the screen to say.
func toastFor(params map[string]string) string {
	if params["status"] == "armed" {
		return "ARMED - PRESS RECORD"
	}
	return ""
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
