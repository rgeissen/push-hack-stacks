package main

// playsclosed.go -- Plays when closed: Stacks keeps playing along with Live
// while another app has the surface (Push Hack Home), so Riffs, following
// Stacks Harmony, plays over Stacks' chords. Off by default: closed Stacks
// is parked, the rule asked for on device ("whenever Stacks is closed it
// still plays the Stacks chords when I hit play").

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
)

const keyPlaysClosed = "plays_closed"

type playsClosed struct {
	on   atomic.Bool
	path string
}

func loadPlaysClosed(dataDir string) *playsClosed {
	p := &playsClosed{path: filepath.Join(dataDir, "plays-closed.json")}
	if b, err := os.ReadFile(p.path); err == nil {
		var v struct{ On bool }
		if json.Unmarshal(b, &v) == nil {
			p.on.Store(v.On)
		}
	}
	return p
}

func (p *playsClosed) set(on bool) {
	p.on.Store(on)
	b, _ := json.Marshal(struct{ On bool }{on})
	if err := os.WriteFile(p.path, b, 0o644); err != nil {
		log.Printf("plays when closed: %v", err)
	}
}

// onOff is the setting as a param value.
func onOff(b bool) string { return map[bool]string{true: "on", false: "off"}[b] }

// setPlaysClosed switches the setting, says so on every surface, and
// parks or unparks a closed Stacks at once.
func (s *surfaceCtl) setPlaysClosed(val string) {
	if s.closed == nil {
		return
	}
	on := val == "on"
	if val == "toggle" {
		on = !s.closed.on.Load()
	}
	s.closed.set(on)
	s.eng.setHostParam(keyPlaysClosed, onOff(on))
	if s.mode() != modeEdit {
		s.set("parked", onOff(!on))
	}
	log.Printf("surface: plays when closed = %s", onOff(on))
	s.scr.Toast(map[bool]string{true: "PLAYS WHEN CLOSED", false: "SILENT WHEN CLOSED"}[on])
}

// parkedWhen is whether Stacks parks in mode m.
func (s *surfaceCtl) parkedWhen(m int) bool {
	return m != modeEdit && (s.closed == nil || !s.closed.on.Load())
}
