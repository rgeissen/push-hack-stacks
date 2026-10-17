package surface

import (
	"image"
	"os"
	"testing"
)

// On device (2026-10-17) the PATTERNS tab's DELETE was the edit row's: it
// deleted the selected chord, seven times, while the slot stayed. The tab
// has its own row now, and nothing on it deletes a chord.
func TestPatternsRowDeletesNoChord(t *testing.T) {
	acts := Actions(TabPatterns)
	modes := map[string]bool{}
	for _, a := range acts {
		if a.Key == "remove" || a.Key == "clear" || a.Key == "pat_delete" {
			t.Errorf("PATTERNS button %q writes %s directly", a.Label, a.Key)
		}
		modes[a.Mode] = true
	}
	if !modes["clear"] || !modes["copy"] {
		t.Errorf("PATTERNS row arms clear and copy: modes %v", modes)
	}
	lit := Scene{PatMode: "clear"}
	for _, a := range acts {
		if a.Mode != "" && a.IsLit(lit, Prog{}) != (a.Mode == "clear") {
			t.Errorf("%s lit %v with clear armed", a.Label, a.IsLit(lit, Prog{}))
		}
	}
}

func TestParseSlots(t *testing.T) {
	s := ParseSlots("5:5:A D E -||4:2:Dm7 G7 Cmaj7 Cmaj7" + "|||||||||||||")
	if s[0].Count != 5 || s[0].Bars != 5 || len(s[0].Names) != 4 || s[0].Names[3] != "-" {
		t.Errorf("slot 1 = %+v", s[0])
	}
	if s[1].Count != 0 || s[2].Names[0] != "Dm7" || s[2].Bars != 2 {
		t.Errorf("slots 2, 3 = %+v %+v", s[1], s[2])
	}
	if e := ParseSlots(""); e[0].Count != 0 { // a module without the key
		t.Errorf("no pat_names: %+v", e[0])
	}
}

// A used slot draws its chords, not one word for every slot.
func TestPatternsDrawTheirChords(t *testing.T) {
	raw, err := os.ReadFile("../upstream/stacks/src/module.json")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ParseManifest(raw)
	params := map[string]string{"prog": axisProg, "patterns": "0005|1", "pat_names": "4:4:A E F#m D||1:2:Am9" + "|||||||||||||"}
	draw := func(names string) *image.NRGBA {
		params["pat_names"] = names
		img := image.NewNRGBA(image.Rect(0, 0, W, H))
		Render(img, Scene{M: m, Params: params, Tab: TabPatterns, Touched: -1, Held: -1, Scroll: -1})
		return img
	}
	a := draw("4:4:A E F#m D||1:2:Am9|||||||||||||")
	b := draw("4:4:A E F#m D||1:2:Dm7|||||||||||||")
	diff := false
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			diff = true
			break
		}
	}
	if !diff {
		t.Error("slot 3 drew the same for Am9 and Dm7: its chords are not on screen")
	}
}
