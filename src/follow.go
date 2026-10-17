package main

// follow.go -- Stacks' Key and Scale follow Push's scale.
//
// Push's Scale button edits the Live Set's scale (Live 12). Stacks runs
// outside Live, so it reads that through push-manager's /api/live/scale,
// which asks the Browser Bridge Remote Script inside Live. One way only,
// Push -> Stacks: with both sides writing, nobody could tell which one a
// change came from. Shift + Scale switches it; turning Stacks' own Key or
// Scale switches it off, so the two never fight.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/rgeissen/push-hack-stacks/dsp"
)

const followEvery = time.Second

// stacksScales are Stacks' scales by pitch-class mask, bit i = i
// semitones above the key -- stacks.c's SCALES[], pinned by
// TestStacksScalesMatchTheCore.
var stacksScales = map[int]string{
	0x0FFF: "Chromatic", 0x0AB5: "Major", 0x05AD: "Minor", 0x06AD: "Dorian",
	0x05AB: "Phrygian", 0x0AD5: "Lydian", 0x06B5: "Mixolydian", 0x056B: "Locrian",
	0x09AD: "HarmMinor", 0x0AAD: "MelMinor", 0x0295: "PentMaj", 0x04A9: "PentMin",
	0x04E9: "Blues",
}

var keyNames = []string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}

// stacksScale finds the Stacks scale with the same notes as Live's, by
// intervals rather than by name: Live calls it "Harmonic Minor", Stacks
// "HarmMinor", and only the notes are sure to agree.
func stacksScale(intervals []int) (string, bool) {
	mask := 0
	for _, i := range intervals {
		if i >= 0 && i < 12 {
			mask |= 1 << i
		}
	}
	name, ok := stacksScales[mask]
	return name, ok
}

// paramStore is the part of the engine the follower needs.
type paramStore interface {
	Get(key string) string
	Set(key, val string)
}

type follower struct {
	pm interface {
		LiveScale() (pmclient.Scale, error)
	}
	ps    paramStore
	path  string           // host.json: survives restarts
	toast func(msg string) // nil in tests

	mu   sync.Mutex
	on   bool
	last string // the last state logged and shown
}

func newFollower(pm interface {
	LiveScale() (pmclient.Scale, error)
}, ps paramStore, dataDir string, toast func(string)) *follower {
	f := &follower{pm: pm, ps: ps, path: filepath.Join(dataDir, "host.json"), toast: toast}
	if b, err := os.ReadFile(f.path); err == nil {
		var h struct {
			FollowScale bool `json:"follow_scale"`
		}
		if json.Unmarshal(b, &h) == nil {
			f.on = h.FollowScale
		}
	}
	return f
}

func (f *follower) On() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on
}

// Set switches following and saves the choice; why goes on the screen.
func (f *follower) Set(on bool, why string) {
	f.mu.Lock()
	changed := f.on != on
	f.on, f.last = on, ""
	f.mu.Unlock()
	if !changed {
		return
	}
	b, _ := json.Marshal(map[string]bool{"follow_scale": on})
	if err := os.WriteFile(f.path, b, 0o644); err != nil {
		log.Printf("follow: save: %v", err)
	}
	log.Printf("follow: Push's scale %s%s", map[bool]string{true: "on", false: "off"}[on], why)
	f.say(map[bool]string{true: "FOLLOWING PUSH'S SCALE", false: "NOT FOLLOWING PUSH'S SCALE"}[on] + why)
	if on {
		f.Sync()
	}
}

func (f *follower) say(msg string) {
	if f.toast != nil {
		f.toast(msg)
	}
}

// note logs and shows a state only when it changed.
func (f *follower) note(state, show string) {
	f.mu.Lock()
	same := state == f.last
	f.last = state
	f.mu.Unlock()
	if same {
		return
	}
	log.Printf("follow: %s", state)
	if show != "" {
		f.say(show)
	}
}

// Sync reads Push's scale once and brings Stacks' Key and Scale to it.
func (f *follower) Sync() {
	if !f.On() {
		return
	}
	sc, err := f.pm.LiveScale()
	switch {
	case err != nil && strings.Contains(err.Error(), "7704"):
		f.note("cannot read Push's scale: Browser Bridge is not active in Live",
			"PUSH'S SCALE: ACTIVATE BROWSER BRIDGE IN LIVE")
		return
	case err != nil:
		f.note("cannot read Push's scale: "+err.Error(), "PUSH'S SCALE: CANNOT READ IT")
		return
	case !sc.Supported:
		f.note("this Live has no scale to read", "THIS LIVE HAS NO SCALE TO READ")
		return
	}
	key := keyNames[((sc.Root%12)+12)%12]
	scale, ok := stacksScale(sc.Intervals)
	if !ok { // follow the key at least, and say what is missing
		f.note("Push's scale "+sc.Name+" is not one of Stacks' scales; following its key only",
			"PUSH'S "+strings.ToUpper(sc.Name)+" IS NOT IN STACKS: KEY ONLY")
		scale = f.ps.Get("scale")
	} else {
		f.note("following "+key+" "+scale, "")
	}
	moved := false
	if f.ps.Get("scale") != scale {
		f.ps.Set("scale", scale)
		moved = true
	}
	if f.ps.Get("key") != key {
		f.ps.Set("key", key)
		moved = true
	}
	if moved {
		log.Printf("follow: Stacks now %s %s, from Push", key, scale)
		f.say("FROM PUSH: " + key + " " + strings.ToUpper(scale))
	}
}

func (f *follower) run(done <-chan struct{}) {
	t := time.NewTicker(followEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			f.Sync()
		}
	}
}

// engineParams is the engine as a paramStore: every call on its goroutine.
type engineParams struct{ eng *Engine }

func (e engineParams) Get(key string) string {
	var v string
	e.eng.Call(func(i *dsp.Instance) { v, _ = i.GetParam(key) }, 500*time.Millisecond)
	return v
}

func (e engineParams) Set(key, val string) {
	e.eng.Call(func(i *dsp.Instance) { i.SetParam(key, val) }, 500*time.Millisecond)
}
