package dsp

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The harmony sender announces chords from SeqAt before they play. Over
// progressions of uneven lengths, start offsets, a muted chord and clips
// longer and shorter than the phrase, a prediction made `ahead` clocks
// early must be the chord the core then arms, at every clock.
func TestSeqAtPredictsTheCore(t *testing.T) {
	Hooks.Notes = func([]byte) {}
	Hooks.Tempo = func() float32 { return 120 }
	defer func() { Hooks.Notes, Hooks.Tempo = nil, nil }()
	if err := Init(); err != nil {
		t.Fatal(err)
	}
	type chord struct {
		deg      string
		len, off int
		mute     bool
	}
	cases := []struct {
		bars   int
		chords []chord
	}{
		{4, []chord{{"I", 8, 0, false}, {"IV", 8, 0, false}, {"V", 8, 0, false}, {"I", 8, 0, false}}},
		{2, []chord{{"II", 4, 0, false}, {"V", 4, 0, false}, {"I", 8, 0, false}}},
		{4, []chord{{"VI", 6, 0, false}, {"IV", 2, 0, false}, {"I", 12, 2, false}, {"V", 4, 1, false}}},
		{3, []chord{{"I", 8, 0, false}, {"VII", 8, 0, true}, {"IV", 8, 3, false}, {"I", 8, 0, false}}},
		{1, []chord{{"I", 16, 0, false}, {"V", 16, 0, false}}}, // the clip cuts the phrase
		{8, []chord{{"I", 3, 0, false}, {"IV", 5, 0, false}, {"V", 8, 4, false}}},
	}
	for ci, c := range cases {
		t.Run(fmt.Sprint(ci), func(t *testing.T) {
			inst, err := New(t.TempDir(), `{"state_path":"`+filepath.Join(t.TempDir(), "s.json")+`"}`)
			if err != nil {
				t.Fatal(err)
			}
			defer inst.Destroy()
			inst.SetParam("clear", "on")
			inst.SetParam("hum_time", "0")
			count := func() int {
				p, _ := inst.GetParam("prog")
				h := strings.Split(p, "|")
				return len(strings.Split(h[len(h)-1], ";"))
			}
			for count() < len(c.chords) { // clear leaves four rests
				inst.SetParam("sel", fmt.Sprint(count()))
				inst.SetParam("insert", "on")
			}
			for count() > len(c.chords) {
				inst.SetParam("sel", fmt.Sprint(count()))
				inst.SetParam("remove", "on")
			}
			for i, ch := range c.chords {
				inst.SetParam("sel", fmt.Sprint(i+1))
				inst.SetParam("degree", ch.deg)
				inst.SetParam("len", fmt.Sprint(ch.len))
				inst.SetParam("off", fmt.Sprint(ch.off))
				if ch.mute {
					inst.SetParam("cmute", "on")
				}
			}
			inst.SetParam("bars", fmt.Sprint(c.bars))
			inst.SetParam("run", "on")
			if n := count(); n != len(c.chords) {
				t.Fatalf("%d chords written, %d wanted", n, len(c.chords))
			}
			const maxAhead = 12
			pred := map[[2]int]int{} // (pulse, ahead) -> idx
			inst.OnMIDI([]byte{0xFA}, SourceExternal)
			checked, changes, last := 0, 0, -2
			for pulse := 0; pulse < 24*4*16; pulse++ {
				inst.OnMIDI([]byte{0xF8}, SourceExternal)
				idx, now := inst.SeqAt(0)
				if now != pulse {
					t.Fatalf("pulse %d: SeqAt says %d", pulse, now)
				}
				for a := 0; a <= maxAhead; a++ {
					got, _ := inst.SeqAt(a)
					pred[[2]int{pulse + a, a}] = got
				}
				for b := 0; b < 7; b++ {
					inst.Render()
				}
				cur := inst.Cur()
				if cur != idx && !(cur == -1 && c.chords[max(idx, 0)].mute) {
					t.Fatalf("pulse %d: armed %d, SeqAt(0) %d", pulse, cur, idx)
				}
				for a := 0; a <= maxAhead; a++ {
					if p, ok := pred[[2]int{pulse, a}]; ok {
						if p != idx {
							t.Fatalf("pulse %d: predicted %d clocks ahead as chord %d, plays %d", pulse, a, p, idx)
						}
						checked++
					}
				}
				if idx != last {
					changes++
					last = idx
				}
			}
			if changes < len(c.chords) {
				t.Fatalf("only %d chord changes: the progression did not play", changes)
			}
			t.Logf("%d predictions checked, %d changes", checked, changes)
		})
	}
}
