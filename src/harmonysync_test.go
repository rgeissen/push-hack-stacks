package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/harmony"
	"github.com/rgeissen/push-hack-stacks/dsp"
	"github.com/rgeissen/push-hack-stacks/surface"
)

// harmonyFollower is what Riffs does with Stacks Harmony, at its simplest: a
// message heard at clock c applies at c + delta.
type harmonyFollower struct {
	pulse   int // the clock count now, -1 before the downbeat
	pending map[int]harmony.Harmony
	now     harmony.Harmony
	has     bool
	msgs    int
	alive   int
}

func (f *harmonyFollower) hear(b []byte) {
	m, ok := harmony.Decode(b)
	if !ok {
		panic(fmt.Sprintf("undecodable: % X", b))
	}
	f.msgs++
	switch m.Type {
	case harmony.TypeAlive:
		f.alive++
	case harmony.TypeHarmony:
		if m.Delta == 0 {
			f.now, f.has = m.Harmony, true
			return
		}
		f.pending[max(f.pulse, 0)+m.Delta] = m.Harmony
	}
}

func (f *harmonyFollower) clock() {
	f.pulse++
	if h, ok := f.pending[f.pulse]; ok {
		f.now, f.has = h, true
		delete(f.pending, f.pulse)
	}
}

type rig struct {
	t    *testing.T
	inst *dsp.Instance
	h    *harmonySender
	f    *harmonyFollower
	rev  string
}

func newRig(t *testing.T) *rig {
	dsp.Hooks.Notes = func([]byte) {}
	dsp.Hooks.Tempo = func() float32 { return 120 }
	t.Cleanup(func() { dsp.Hooks.Notes, dsp.Hooks.Tempo = nil, nil })
	if err := dsp.Init(); err != nil {
		t.Fatal(err)
	}
	inst, err := dsp.New(t.TempDir(), `{"state_path":"`+filepath.Join(t.TempDir(), "s.json")+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(inst.Destroy)
	r := &rig{t: t, inst: inst, f: &harmonyFollower{pulse: -1, pending: map[int]harmony.Harmony{}}}
	r.h = &harmonySender{nowIdx: -1, pendIdx: -1, nextAhead: -2, send: r.f.hear}
	return r
}

// poll is the engine's: the whole state when its rev moved.
func (r *rig) poll() {
	rui, _ := r.inst.GetParam("rui_poll")
	rev, _, _ := strings.Cut(rui, ":")
	changed := map[string]string{}
	if rev != r.rev {
		r.rev = rev
		s, _ := r.inst.GetParam("state")
		json.Unmarshal([]byte(s), &changed)
	}
	r.h.poll(r.inst, changed, time.Now())
}

func (r *rig) midi(b byte) {
	r.inst.OnMIDI([]byte{b}, dsp.SourceExternal)
	r.h.midi(r.inst, b)
	if b == 0xFA || b == 0xFC {
		r.f.pulse = -1
	}
}

// clock: one pulse as the engine runs it, the follower hearing the clock
// first (the worse order for it: messages then arrive "late").
func (r *rig) clock() {
	r.f.clock()
	r.inst.OnMIDI([]byte{0xF8}, dsp.SourceExternal)
	r.h.midi(r.inst, 0xF8)
	for b := 0; b < 7; b++ {
		r.inst.Render()
	}
	r.h.afterRender(r.inst)
}

// sounding is the chord the core armed, as harmony, ok false for none or
// a rest.
func (r *rig) sounding() (harmony.Harmony, bool) {
	cur := r.inst.Cur()
	if cur < 0 || !r.h.progOK {
		return harmony.Harmony{}, false
	}
	return r.h.of(cur)
}

func (r *rig) write(bars int, degs []string, lens []int) {
	r.inst.SetParam("clear", "on")
	r.inst.SetParam("hum_time", "0")
	r.inst.SetParam("key", "C")
	r.inst.SetParam("scale", "Major")
	count := func() int {
		p, _ := r.inst.GetParam("prog")
		h := strings.Split(p, "|")
		return len(strings.Split(h[len(h)-1], ";"))
	}
	for count() < len(degs) {
		r.inst.SetParam("sel", fmt.Sprint(count()))
		r.inst.SetParam("insert", "on")
	}
	for count() > len(degs) {
		r.inst.SetParam("sel", fmt.Sprint(count()))
		r.inst.SetParam("remove", "on")
	}
	for i, d := range degs {
		r.inst.SetParam("sel", fmt.Sprint(i+1))
		r.inst.SetParam("degree", d)
		r.inst.SetParam("len", fmt.Sprint(lens[i]))
	}
	r.inst.SetParam("bars", fmt.Sprint(bars))
	r.inst.SetParam("run", "on")
	r.poll()
}

// The follower knows the chord on every clock it sounds: announced before
// it, applied on the clock it starts -- not one clock late.
func TestFollowerIsOnTheChordEveryClock(t *testing.T) {
	r := newRig(t)
	r.write(4, []string{"I", "VI", "IV", "V"}, []int{8, 4, 4, 16})
	r.midi(0xFA)
	if !r.f.has || !r.f.now.Armed && !r.f.now.Playing {
		t.Fatal("no chord at Start")
	}
	changes, last := 0, -1
	for p := 0; p < 24*4*8; p++ {
		r.clock()
		if p%24 == 0 {
			r.poll()
		}
		want, ok := r.sounding()
		if !ok {
			continue
		}
		if !r.f.now.Same(want) {
			t.Fatalf("clock %d: follower on %s, Stacks plays %s",
				p, harmony.Name(r.f.now.Root, r.f.now.Chord, r.f.now.Bass), harmony.Name(want.Root, want.Chord, want.Bass))
		}
		if !r.f.now.Playing {
			t.Fatalf("clock %d: not marked playing", p)
		}
		if c := r.inst.Cur(); c != last {
			changes, last = changes+1, c
			t.Logf("clock %d: chord %d %s", p, c, harmony.Name(want.Root, want.Chord, want.Bass))
		}
	}
	if changes < 7 { // I vi IV V, twice
		t.Fatalf("only %d changes", changes)
	}
	if r.f.alive > 0 {
		t.Log("alive messages while busy:", r.f.alive)
	}
	// Stop: followers learn the chord a Start plays, marked armed
	r.midi(0xFC)
	if !r.f.now.Armed || r.f.now.Playing || r.f.now.Root != 0 && r.h.prog.Key == 0 {
		t.Fatalf("after Stop: %+v", r.f.now)
	}
	t.Logf("%d messages, %d chord changes", r.f.msgs, changes)
}

// An edit while playing reaches the follower: the chord that sounds, and
// the one coming.
func TestEditWhilePlaying(t *testing.T) {
	r := newRig(t)
	r.write(2, []string{"I", "IV"}, []int{8, 8})
	r.midi(0xFA)
	for p := 0; p < 24; p++ { // a beat into chord 1
		r.clock()
	}
	r.inst.SetParam("sel", "1")
	r.inst.SetParam("degree", "II") // the sounding chord changes
	r.poll()
	p, _ := r.inst.GetParam("prog")
	t.Log(p[strings.LastIndex(p, "|")+1:])
	want, _ := r.sounding()
	if !r.f.now.Same(want) || r.f.now.Root != 2 {
		t.Fatalf("edited sounding chord: follower %+v, want %+v", r.f.now, want)
	}
	r.inst.SetParam("sel", "2")
	r.inst.SetParam("degree", "V") // the next one changes
	r.poll()
	for p := 24; p < 24*8; p++ {
		r.clock()
		if want, ok := r.sounding(); ok && !r.f.now.Same(want) {
			t.Fatalf("clock %d: follower %+v, Stacks %+v", p, r.f.now, want)
		}
	}
}

// Run off (or parked): Stacks plays nothing, and says so.
func TestRunOffIsSilent(t *testing.T) {
	r := newRig(t)
	r.write(1, []string{"I"}, []int{8})
	r.midi(0xFA)
	for p := 0; p < 30; p++ {
		r.clock()
	}
	if !r.f.now.Live() {
		t.Fatal("not live while playing")
	}
	r.inst.SetParam("run", "off")
	r.poll()
	r.clock()
	if r.f.now.Live() {
		t.Fatalf("Run off, still followed: %+v", r.f.now)
	}
}

// The chord set is what sounds: the notes, the root, the bass.
func TestHarmonyOfAChord(t *testing.T) {
	h := &harmonySender{progOK: true, armedRun: true, prog: surface.Prog{Key: 0, ScaleMask: 0xAB5,
		Chords: []surface.Chord{{Name: "Am7", Root: "A3", Notes: []int{52, 57, 60, 67}}, {Name: "REST"}}}}
	hm, ok := h.of(0)
	if !ok || hm.Root != 9 || hm.Bass != 4 || hm.Chord != harmony.Rotate(1<<4|1<<9|1<<0|1<<7, 9) || !hm.Armed {
		t.Fatalf("Am7/E: %+v", hm)
	}
	if _, ok := h.of(1); ok {
		t.Fatal("a rest has a harmony")
	}
}
