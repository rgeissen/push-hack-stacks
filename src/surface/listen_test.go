package surface

import (
	"image"
	"testing"
	"time"
)

func TestKeyWindowMovesByOctavesOnly(t *testing.T) {
	held := func(ns ...int) []ListenNote {
		var out []ListenNote
		for _, n := range ns {
			out = append(out, ListenNote{Note: n, On: time.Now()})
		}
		return out
	}
	if w := KeyWindow(0, nil); w != 48 {
		t.Errorf("fresh window %d, want C3 (48)", w)
	}
	if w := KeyWindow(48, held(60, 64, 67)); w != 48 {
		t.Errorf("a chord inside moved the keyboard to %d", w)
	}
	if w := KeyWindow(48, held(96, 100)); w != 60 {
		t.Errorf("notes above: window %d, want 60 (one octave up, 4 octaves to C8)", w)
	}
	if w := KeyWindow(48, held(40)); w != 36 {
		t.Errorf("a note below: window %d, want 36", w)
	}
	released := []ListenNote{{Note: 100, On: time.Now(), Off: time.Now()}}
	if w := KeyWindow(48, released); w != 48 {
		t.Errorf("a released note moved the keyboard to %d", w)
	}
}

func TestBoardText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inv, bass int
		known     bool
		want      string
	}{
		{"Cmaj7", 0, 0, true, DisplayName("Cmaj7")},
		{"C", 1, 4, true, "C/E"},
		{"C", 0, 0, false, "?"},
		{"", 0, 0, true, ""},
	} {
		if got := BoardText(tc.name, tc.inv, tc.bass, tc.known); got != tc.want {
			t.Errorf("%q inv %d: %q, want %q", tc.name, tc.inv, got, tc.want)
		}
	}
}

// Motion is reported while it lasts and not after: the screen redraws at
// 30 fps only while something moves.
func TestRenderListenSaysWhenItMoves(t *testing.T) {
	t0 := time.Now()
	img := image.NewNRGBA(image.Rect(0, 0, W, H))
	sc := ListenScene{Notes: []ListenNote{{Note: 60, On: t0}, {Note: 64, On: t0}, {Note: 67, On: t0}},
		Name: "C", NameAt: t0, Root: 0, Bass: 0, Known: true, KeysLo: 48, Key: 0, Mask: 0xAB5}
	for _, tc := range []struct {
		after  time.Duration
		moving bool
	}{{0, true}, {100 * time.Millisecond, true}, {2 * time.Second, false}} {
		sc.Now = t0.Add(tc.after)
		if got := RenderListen(img, sc); got != tc.moving {
			t.Errorf("%v after the press: moving %v, want %v", tc.after, got, tc.moving)
		}
	}
	sc.Notes[0].Off = t0.Add(3 * time.Second) // a release fades, then stops
	sc.Now = t0.Add(3*time.Second + 50*time.Millisecond)
	if !RenderListen(img, sc) {
		t.Error("a fading release does not move")
	}
	sc.Now = t0.Add(5 * time.Second)
	if RenderListen(img, sc) {
		t.Error("still moving long after the release")
	}
	// Idle, with and without a problem to say: no panic, no motion.
	if RenderListen(img, ListenScene{Now: t0, Root: -1, Bass: -1, KeysLo: 48, Status: "ACTIVATE BROWSER BRIDGE"}) {
		t.Error("idle moves")
	}
}
