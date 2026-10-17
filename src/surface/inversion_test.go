package surface

import "testing"

// Your set on device (2026-10-17): D . E7 . Amaj7, grouping dynamic, in D
// major. Voicings measured off the real core with E7's inversion stepped.
func progE7(e7 string) string {
	return "v13|3|2|-1|8|ab5|2|32|0|4|120|0|1|" +
		"D,D3,0,8,0,100,0,1,38:100.42:100.45:100;" +
		"E7,E4,0,8,0,100,0,1," + e7 + ";" +
		"Amaj7,A3,0,16,0,100,0,1,37:100.40:100.44:100.45:100"
}

const (
	e7Root = "38:100.40:100.44:100.47:100" // inv 0, -1 and +3 all give this
	e7Up   = "38:100.44:100.47:100.52:100" // inv +1 and -3
	e7Down = "35:100.38:100.40:100.44:100" // inv +2 and -2
)

func TestInvAuto(t *testing.T) {
	p, _ := ParseProg(progE7(e7Root))
	for i, want := range []bool{false, true, true} { // the first chord leads from nothing
		if got := p.InvAuto(i, "dynamic"); got != want {
			t.Errorf("chord %d under dynamic: auto %v, want %v", i+1, got, want)
		}
	}
	if !p.InvAuto(1, "dyn +1oct") || !p.InvAuto(1, "dyn +2oct") {
		t.Error("the lifted dynamic groupings lead too")
	}
	if p.InvAuto(1, "none") || p.InvAuto(1, "C2-B3") || p.InvAuto(1, "drop 2") {
		t.Error("only the dynamic groupings pick the inversion")
	}
	p.Chords[0].Notes = nil // a rest: the next chord leads from nothing
	if p.InvAuto(1, "dynamic") {
		t.Error("a chord after a rest keeps its inversion")
	}
}

// progOf writes a v13 prog of one-bar chords from their notes, in D major.
func progOf(chords ...[]int) string {
	s := "v13|" + itoa(len(chords)) + "|1|-1|8|ab5|2|32|0|4|120|0|0|"
	for i, c := range chords {
		if i > 0 {
			s += ";"
		}
		s += "X,D3,0,8,0,100,0,1,"
		for j, n := range c {
			if j > 0 {
				s += "."
			}
			s += itoa(n) + ":100"
		}
	}
	return s
}

// The voicings the real core played (2026-10-17) for I-vi-IV-V in D with
// grouping none, while one chord's inversion at a time was stepped:
// chord 2 up three detents, chord 3 down two, chord 1 and 4 up, and every
// one back to 0 -- the sweep that showed the lanes jumping on device.
var sweep = [][][]int{
	{{57, 61, 64}, {65, 69, 72}, {62, 66, 69}, {64, 68, 71}},
	{{57, 61, 64}, {69, 72, 77}, {62, 66, 69}, {64, 68, 71}},
	{{57, 61, 64}, {72, 77, 81}, {62, 66, 69}, {64, 68, 71}},
	{{57, 61, 64}, {77, 81, 84}, {62, 66, 69}, {64, 68, 71}},
	{{57, 61, 64}, {77, 81, 84}, {57, 62, 66}, {64, 68, 71}},
	{{57, 61, 64}, {77, 81, 84}, {54, 57, 62}, {64, 68, 71}},
	{{64, 69, 73}, {77, 81, 84}, {54, 57, 62}, {64, 68, 71}},
	{{64, 69, 73}, {77, 81, 84}, {54, 57, 62}, {76, 80, 83}},
	{{64, 69, 73}, {65, 69, 72}, {54, 57, 62}, {76, 80, 83}},
	{{64, 69, 73}, {65, 69, 72}, {62, 66, 69}, {76, 80, 83}},
	{{57, 61, 64}, {65, 69, 72}, {62, 66, 69}, {76, 80, 83}},
	{{57, 61, 64}, {65, 69, 72}, {62, 66, 69}, {64, 68, 71}},
}

func lanesOf(t *testing.T, chords [][]int) Prog {
	p, ok := ParseProg(progOf(chords...))
	if !ok {
		t.Fatal("progOf wrote a prog that does not parse")
	}
	return p
}

// rows is where each note of every chord lands, in pixels.
func rows(p Prog, w LaneRange) [][]int {
	st := newStaff(p, "scale", w)
	var out [][]int
	for _, c := range p.Chords {
		var r []int
		for _, n := range c.Notes {
			r = append(r, st.laneY(st.degree[n]))
		}
		out = append(out, r)
	}
	return out
}

// The rules that make the staff consistent, checked frame by frame over
// the device sweep, twice over (the second pass is a user going on).
func TestLanesHoldStillThroughASweep(t *testing.T) {
	var w LaneRange
	var prevRows [][]int
	zooms := 0
	for pass := 0; pass < 2; pass++ {
		for f, chords := range sweep {
			p := lanesOf(t, chords)
			next := FitLanes(p, "scale", w)
			st := newStaff(p, "scale", next)
			for i, c := range p.Chords { // every note on screen
				for _, n := range c.Notes {
					if d := st.degree[n]; d <= next.Lo || d >= next.Hi {
						t.Errorf("pass %d frame %d chord %d note %d outside lanes %d..%d (no air)", pass, f, i+1, n, next.Lo, next.Hi)
					}
				}
			}
			r := rows(p, next)
			if w.Step != 0 {
				if next.Step != w.Step {
					zooms++
					if next.Step > w.Step && pass == 0 && f < len(sweep)-1 {
						t.Errorf("frame %d zoomed IN mid-sweep (%d -> %d px)", f, w.Step, next.Step)
					}
				}
				// A chord the edit did not touch keeps its rows unless the
				// zoom changed or the window panned.
				if next == w {
					for i := range chords {
						if equalInts(chords[i], sweep[(f+len(sweep)-1)%len(sweep)][i]) && !equalInts(r[i], prevRows[i]) {
							t.Errorf("frame %d: untouched chord %d moved %v -> %v", f, i+1, prevRows[i], r[i])
						}
					}
				}
			}
			if pass == 1 && w.Step != 0 && next != w {
				t.Errorf("second pass frame %d: the window still moves %+v -> %+v", f, w, next)
			}
			w, prevRows = next, r
		}
	}
	if zooms > 1 {
		t.Errorf("%d zoom changes over the sweep; each one rescales every chord", zooms)
	}
}

// A new harmony (here: chord 2 becomes another chord, notes unchanged in
// span) may re-frame to the zoom that fits; the same notes reached by
// voicing edits may not.
func TestLanesZoomInOnlyForANewHarmony(t *testing.T) {
	wide := lanesOf(t, sweep[5]) // the widest frame of the sweep
	w := FitLanes(wide, "scale", LaneRange{})
	narrow := lanesOf(t, sweep[0])
	if v := FitLanes(narrow, "scale", w); v.Step != w.Step {
		t.Errorf("a voicing edit back to a narrow span zoomed %d -> %d px", w.Step, v.Step)
	}
	narrow.Chords[1].Name = "Em"
	if h := FitLanes(narrow, "scale", w); h.Step <= w.Step {
		t.Errorf("a new harmony with room to spare kept %d px; want a larger step", h.Step)
	}
}

// Moving the whole progression an octave (Octave Default) pans by exactly
// an octave when it must, so the tonic lines keep their rows; and inside
// a window with room, the chords visibly move.
func TestLanesPanByOctaves(t *testing.T) {
	base := [][]int{{57, 61, 64}, {62, 66, 69}}
	p := lanesOf(t, base)
	w := FitLanes(p, "scale", LaneRange{})
	up := make([][]int, len(base))
	for i, c := range base {
		for _, n := range c {
			up[i] = append(up[i], n+12)
		}
	}
	pu := lanesOf(t, up)
	w2 := FitLanes(pu, "scale", w)
	if w2.Step != w.Step {
		t.Fatalf("an octave shift changed the zoom %d -> %d", w.Step, w2.Step)
	}
	st := lanesFor(p, "scale")
	octave := st.degree[72] - st.degree[60]
	if d := w2.Lo - w.Lo; d != 0 && d%octave != 0 {
		t.Errorf("panned %d lanes for an octave shift; want a whole octave (%d)", d, octave)
	}
	if w2.Lo == w.Lo && rows(p, w)[0][0] == rows(pu, w2)[0][0] {
		t.Error("the octave shift neither moved the chords nor the window")
	}
}

func TestLanesRefitOnKeyChange(t *testing.T) {
	p := lanesOf(t, sweep[3])
	w := FitLanes(p, "scale", LaneRange{})
	p.Key = 5
	if k := FitLanes(p, "scale", w); k.Key != 5 {
		t.Errorf("key change kept the old window %+v", k)
	}
	if f := FitLanes(lanesOf(t, sweep[0]), "scale", LaneRange{}); f.Step != fitStep(f.Hi-f.Lo) {
		t.Errorf("a fresh fit is the largest step that shows its window: %+v", f)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
