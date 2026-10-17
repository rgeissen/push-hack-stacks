package dsp

import (
	"path/filepath"
	"testing"
)

// A held chord pad sets `audition` on (control.go): with the transport
// stopped, Stacks must loop the selected chord WITH its rhythm until it
// goes off -- reported on device as "playing a rhythm with the lower chord
// pads doesn't play the rhythm at all" while the pad sent held_pad notes.
// "quarter" on a one-bar chord at 120 BPM: a strike every 0.5 s.
func TestAuditionLoopsTheRhythm(t *testing.T) {
	block := 0
	var strikes []int // block of each note-on group
	sounding := map[byte]bool{}
	Hooks.Notes = func(m []byte) {
		switch {
		case m[0]&0xF0 == 0x90 && m[2] > 0:
			if len(strikes) == 0 || strikes[len(strikes)-1] < block-5 {
				strikes = append(strikes, block)
			}
			sounding[m[1]] = true
		case m[0]&0xF0 == 0x80 || m[0]&0xF0 == 0x90:
			delete(sounding, m[1])
		}
	}
	Hooks.Tempo = func() float32 { return 120 }
	defer func() { Hooks.Notes, Hooks.Tempo = nil, nil }()

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	inst, err := New(t.TempDir(), `{"state_path":"`+filepath.Join(t.TempDir(), "s.json")+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Destroy()
	inst.SetParam("clear", "on")
	inst.SetParam("hum_time", "0")
	for i, deg := range []string{"I", "IV", "V", "I"} {
		inst.SetParam("sel", string(rune('1'+i)))
		inst.SetParam("degree", deg)
	}
	inst.SetParam("sel", "2")
	inst.SetParam("rhythm", "quarter")
	for ; block < 50; block++ { // the edit's own audition, if any, dies out
		inst.Render()
	}
	strikes = nil

	inst.SetParam("audition", "on")
	start := block
	perBar := 2 * SampleRate / FramesPerBlock // 120 BPM: a bar is 2 s
	for ; block < start+2*perBar+perBar/8; block++ {
		inst.Render()
	}
	if len(strikes) != 9 { // 4 a bar for two bars, and the third bar's first
		t.Fatalf("held two bars: %d strikes at blocks %v, want 9", len(strikes), strikes)
	}
	for i := 1; i < len(strikes); i++ {
		gap := strikes[i] - strikes[i-1]
		if d := gap - perBar/4; d < -2 || d > 2 {
			t.Errorf("strike %d came %d blocks after the last, want %d (a quarter)", i, gap, perBar/4)
		}
	}

	inst.SetParam("audition", "off")
	n := len(strikes)
	for end := block + perBar; block < end; block++ {
		inst.Render()
	}
	if len(strikes) != n {
		t.Errorf("released: %d more strikes", len(strikes)-n)
	}
	if len(sounding) != 0 {
		t.Errorf("released: notes %v still sounding", sounding)
	}
}
