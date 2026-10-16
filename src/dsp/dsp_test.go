package dsp

import (
	"os"
	"path/filepath"
	"testing"
)

// The vendored core, driven through the Push host exactly as the engine
// drives it: Start, then Live's clock at 24 pulses per beat, with the
// blocks that fall between pulses at 120 BPM. With Run on, Stacks plays
// its progression on the incoming transport, so notes must come out.
func TestClockDrivesNotes(t *testing.T) {
	var notes [][]byte
	Hooks.Notes = func(m []byte) { notes = append(notes, append([]byte(nil), m...)) }
	Hooks.Tempo = func() float32 { return 120 }
	Hooks.Log = func(m string) { t.Log(m) }
	defer func() { Hooks.Notes, Hooks.Tempo, Hooks.Log = nil, nil, nil }()

	if err := Init(); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state.json")
	inst, err := New(t.TempDir(), `{"state_path":"`+state+`"}`)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := inst.GetParam("module_id"); !ok || got != "stacks" {
		t.Fatalf("module_id = %q, %v", got, ok)
	}
	// A fresh Stacks is four rests; write I-IV-V-I as the upstream
	// tool-inject test does.
	inst.SetParam("clear", "on")
	inst.SetParam("hum_time", "0")
	for i, deg := range []string{"I", "IV", "V", "I"} {
		inst.SetParam("sel", string(rune('1'+i)))
		inst.SetParam("degree", deg)
	}
	inst.SetParam("run", "on")

	// 120 BPM: a pulse every 20.8 ms, ~7 blocks of 2.9 ms.
	inst.OnMIDI([]byte{0xFA}, SourceExternal)
	for pulse := 0; pulse < 24*8; pulse++ {
		inst.OnMIDI([]byte{0xF8}, SourceExternal)
		for b := 0; b < 7; b++ {
			inst.Render()
		}
	}
	inst.OnMIDI([]byte{0xFC}, SourceExternal)
	for b := 0; b < 50; b++ {
		inst.Render()
	}

	on, off := 0, 0
	for _, m := range notes {
		switch {
		case m[0]&0xF0 == 0x90 && m[2] > 0:
			on++
		case m[0]&0xF0 == 0x80 || m[0]&0xF0 == 0x90:
			off++
		}
		if m[0]&0xF0 != 0x80 && m[0]&0xF0 != 0x90 {
			t.Errorf("non-note output % x: the tool injects notes only", m)
		}
	}
	if on == 0 {
		t.Fatalf("no notes in 8 beats of clock (got %d messages)", len(notes))
	}
	if off < on {
		t.Errorf("%d note-ons but %d note-offs after Stop: a note was left sounding", on, off)
	}
	t.Logf("%d note-ons, %d note-offs", on, off)

	inst.Destroy() // saves synchronously
	if _, err := os.Stat(state); err != nil {
		t.Errorf("state not saved on destroy: %v", err)
	}
}
