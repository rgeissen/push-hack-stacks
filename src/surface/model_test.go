package surface

import (
	"os"
	"reflect"
	"testing"
)

// Captured from the device (Stacks on Push, 2026-10-16): Axis, chord 2
// selected, in A with the scale left at Minor.
const axisProg = "v13|4|2|-1|8|5ad|9|32|0|4|120|0|0|" +
	"A,A3,0,8,0,100,0,1,57:100.61:100.64:100;E,E4,0,8,0,100,0,1,64:100.68:100.71:100;" +
	"F#m,F#4,0,8,0,100,0,1,66:100.69:100.73:100;D,D4,0,8,0,100,0,1,62:100.66:100.69:100"

func TestParseProgDevice(t *testing.T) {
	p, ok := ParseProg(axisProg)
	if !ok {
		t.Fatal("did not parse")
	}
	if p.Sel != 2 || p.Playing != -1 || p.Key != 9 || p.ClipUnits != 32 || len(p.Chords) != 4 {
		t.Fatalf("header: %+v", p)
	}
	c := p.Chords[2]
	if c.Name != "F#m" || c.Root != "F#4" || c.Len != 8 || !reflect.DeepEqual(c.Notes, []int{66, 69, 73}) {
		t.Errorf("chord 3: %+v", c)
	}
	if p.MusicUnits() != 32 || !reflect.DeepEqual(p.Starts(), []int{0, 8, 16, 24}) {
		t.Errorf("timing: %d %v", p.MusicUnits(), p.Starts())
	}
}

func TestParseProgRejects(t *testing.T) {
	for _, s := range []string{"", "v11|4", "garbage", "v13|1|2"} {
		if _, ok := ParseProg(s); ok {
			t.Errorf("%q parsed", s)
		}
	}
	// A rest carries no notes, not a note at pitch 0.
	p, _ := ParseProg("v13|1|1|-1|8|5ad|9|8|0|4|120|0|0|REST,A4,0,8,0,100,0,1,")
	if !p.Chords[0].Rest() {
		t.Error("an empty note list must be a rest")
	}
}

func TestDisplayName(t *testing.T) {
	cases := map[string]string{
		"A": "A", "F#m": "F#MIN", "C#m7": "C#MIN7", "Amaj7": "AMAJ7", "Am9": "AMIN9",
		"Bm7b5": "BMIN7-5", "G7b9": "G7-9", "G7#9": "G7+9", "CmMaj7": "CMINMAJ7",
		"Dsus4": "DSUS4", "Edim7": "EDIM7", "Bbmaj7": "BBMAJ7", "Fmaj7#5": "FMAJ7+5",
		"Cadd9": "CADD9", "Cmadd9": "CMINADD9", "": "",
	}
	for in, want := range cases {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFunctionAndDegree(t *testing.T) {
	p, _ := ParseProg(axisProg) // A minor: A B C D E F G
	want := []struct {
		f     Function
		roman string
		acc   int
	}{
		{FuncTonic, "I", 0},        // A
		{FuncDominant, "V", 0},     // E
		{FuncOutside, "VI", 0},     // F# is not in A minor; against the major reference it is VI
		{FuncPredominant, "IV", 0}, // D
	}
	for i, c := range p.Chords {
		f := FunctionOf(c, p.Key, p.ScaleMask)
		r, acc := Degree(c, p.Key, p.ScaleMask)
		if f != want[i].f || r != want[i].roman {
			t.Errorf("chord %d %s: function %v roman %s%+d, want %v %s", i+1, c.Name, f, r, acc, want[i].f, want[i].roman)
		}
	}
	// In A minor, C is III (the scale's own third), not bIII.
	if r, acc := Degree(Chord{Root: "C4", Notes: []int{60}}, 9, 0x5ad); r != "III" || acc != 0 {
		t.Errorf("C in A minor = %s%+d, want III", r, acc)
	}
	// Chromatic: Bb in A minor is bII against the major reference.
	if r, acc := Degree(Chord{Root: "A#3", Notes: []int{58}}, 9, 0x5ad); r != "II" || acc != -1 {
		t.Errorf("A# in A minor = %s%+d, want bII", r, acc)
	}
}

func TestHitsTileTheBar(t *testing.T) {
	hold := Chord{Len: 16, Mask: 1}
	if got := Hits(hold, 0, 8); !reflect.DeepEqual(got, []float64{0}) {
		t.Errorf("hold over two bars = %v, want one strike", got)
	}
	// Quarter notes (sixteenths 0,4,8,12) over two bars: 8 hits, 2 units apart.
	q := Chord{Len: 16, Mask: 0x1111}
	if got := Hits(q, 0, 8); len(got) != 8 || got[1] != 2 {
		t.Errorf("quarters = %v", got)
	}
	// A chord starting on beat 3 shows the second half of its bar's figure.
	if got := Hits(Chord{Len: 4, Mask: 0x1111}, 4, 8); !reflect.DeepEqual(got, []float64{0, 2}) {
		t.Errorf("phased quarters = %v, want [0 2]", got)
	}
}

func TestSpansFillTheWidth(t *testing.T) {
	mk := func(lens ...int) Prog {
		var p Prog
		for _, l := range lens {
			p.Chords = append(p.Chords, Chord{Len: l})
		}
		return p
	}
	// Four equal chords: a quarter each, edge to edge.
	if got := Spans(mk(8, 8, 8, 8)); !reflect.DeepEqual(got, []int{0, 240, 480, 720, 960}) {
		t.Errorf("4 equal = %v", got)
	}
	// Lengths stay proportional: 16 then 8 is two thirds and one third.
	if got := Spans(mk(16, 8)); got[1] != 640 || got[2] != 960 {
		t.Errorf("2:1 = %v", got)
	}
	// A short chord among long ones is held at the floor, not crushed.
	got := Spans(mk(64, 2, 64))
	if w := got[2] - got[1]; w < minChordW {
		t.Errorf("short chord %dpx, want >= %d (%v)", w, minChordW, got)
	}
	if got[3] != W {
		t.Errorf("does not reach the edge: %v", got)
	}
	// Sixteen chords: a sixteenth each.
	if got := Spans(mk(8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8)); got[1] != 60 || got[16] != W {
		t.Errorf("16 equal = %v", got[:3])
	}
}

// Stacks keeps the last played index after a stop, and a held pad sounds
// with running=0 (both measured on device).
func TestSounding(t *testing.T) {
	stopped, _ := ParseProg("v13|4|1|0|8|5ad|9|32|0|4|120|0|0|A,A3,0,8,0,100,0,1,57:100")
	if got := stopped.Sounding(-1); got != -1 {
		t.Errorf("stopped, nothing held: sounding %d, want -1", got)
	}
	held, _ := ParseProg("v13|4|2|1|8|5ad|9|32|0|4|120|0|0|A,A3,0,8,0,100,0,1,57:100")
	if got := held.Sounding(1); got != 1 {
		t.Errorf("pad holding chord 2: sounding %d, want 1", got)
	}
	if got := held.Sounding(3); got != -1 {
		t.Errorf("a different pad held: sounding %d, want -1", got)
	}
	running, _ := ParseProg("v13|4|1|2|8|5ad|9|32|1|4|120|0|0|A,A3,0,8,0,100,0,1,57:100")
	if got := running.Sounding(-1); got != 2 {
		t.Errorf("running: sounding %d, want 2", got)
	}
}

// The FEEL page's rhythm row has 35 options: by default it follows the
// value; Page Left/Right set a start, clamped so the row stays full.
func TestLongRowScroll(t *testing.T) {
	raw, err := os.ReadFile("../upstream/stacks/src/module.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"rhythm": "bossa", "prog": axisProg}
	follow := Scene{M: m, Params: params, Scroll: -1}
	row, start, total, ok := LongRow(follow, PageFeel)
	if !ok || row != 2 || total != 35 {
		t.Fatalf("FEEL long row = row %d total %d ok %v, want row 2 of 35", row, total, ok)
	}
	if start != 9 { // bossa is option 12; three before it stay on the row
		t.Errorf("following bossa: start %d, want 9", start)
	}
	if _, start, _, _ := LongRow(Scene{M: m, Params: params, Scroll: 8}, PageFeel); start != 8 {
		t.Errorf("scrolled to 8: start %d", start)
	}
	if _, start, _, _ := LongRow(Scene{M: m, Params: params, Scroll: 99}, PageFeel); start != 27 {
		t.Errorf("scrolled past the end: start %d, want 27 (35-8)", start)
	}
	g := Pads(Scene{M: m, Params: params, Scroll: 8, Held: -1}, PageFeel)
	if g[2][0].Value != "tresillo" { // rhythm option 8
		t.Errorf("first rhythm pad at scroll 8 = %q, want tresillo", g[2][0].Value)
	}
	if _, _, _, ok := LongRow(Scene{M: m, Params: map[string]string{"family": "triad"}, Scroll: -1}, PageWrite); ok {
		t.Error("a triad's six shapes fit one row; WRITE has no long row then")
	}
}

// Ground truth from the device (2026-10-17): each rhythm set on chord 2 of
// Am13 · E · F#m · D (chord 2 starts at unit 8, one bar long), one lap
// played into Live's clock, note-ons recorded in units from the chord's
// onset. The mask is the decimal value `prog` publishes.
func TestHitsMatchWhatStacksPlays(t *testing.T) {
	cases := []struct {
		rhythm, mask string
		played       []float64
	}{
		{"quarter", "4369", []float64{0, 2, 4, 6}},
		{"son 3-2", "5193", []float64{0, 1.5, 3, 5, 6}},
		{"offbeat", "17476", []float64{1, 3, 5, 7}},
		{"tresillo", "18761", []float64{0, 1.5, 3, 4, 5.5, 7}},
	}
	for _, c := range cases {
		prog := "v13|4|2|-1|8|ab5|0|32|0|4|120|0|1|" +
			"Am13,A3,0,8,0,100,0,1,45:100;E,E4,0,8,0,100,0," + c.mask + ",52:100.56:100.59:100"
		p, ok := ParseProg(prog)
		if !ok {
			t.Fatalf("%s: prog did not parse", c.rhythm)
		}
		got := Hits(p.Chords[1], p.Starts()[1], p.StepUnits)
		if !reflect.DeepEqual(got, c.played) {
			t.Errorf("%s (mask %s): drawn hits %v, played %v", c.rhythm, c.mask, got, c.played)
		}
	}
}
