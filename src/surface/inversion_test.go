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

func TestFitLanesHoldsStill(t *testing.T) {
	at := func(e7 string, prev LaneRange) LaneRange {
		p, _ := ParseProg(progE7(e7))
		return FitLanes(p, "scale", prev)
	}
	root := at(e7Root, LaneRange{})
	up := at(e7Up, root)
	if up == root {
		t.Fatal("E7 inverted up past the window's top: the window must grow")
	}
	if back := at(e7Root, up); back != up {
		t.Errorf("E7 back to root position inside the grown window: lanes moved %+v -> %+v", up, back)
	}
	down := at(e7Down, up)
	if down.Lo >= up.Lo {
		t.Errorf("E7 down below the window: Lo %d, want under %d", down.Lo, up.Lo)
	}
	if again := at(e7Up, down); again != down {
		t.Errorf("every voicing seen fits now: lanes moved %+v -> %+v", down, again)
	}
	// A key change re-counts the degrees: fit afresh.
	p, _ := ParseProg(progE7(e7Root))
	p.Key = 5
	if k := FitLanes(p, "scale", down); k.Key != 5 {
		t.Errorf("key change kept the old window %+v", k)
	}
}
