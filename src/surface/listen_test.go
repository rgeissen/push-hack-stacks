package surface

import (
	"bytes"
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
		name       string
		root, bass int
		known      bool
		want       string
	}{
		{"Cmaj7", 0, 0, true, DisplayName("Cmaj7")},
		{"C", 0, 4, true, "CMAJ/E"},  // an inversion: root C, bass E
		{"C", 0, 6, true, "CMAJ/F#"}, // a bass outside the chord
		{"C", 0, 0, false, "?"},
		{"", 0, 0, true, ""},
	} {
		if got := BoardText(tc.name, tc.root, tc.bass, tc.known); got != tc.want {
			t.Errorf("%q bass %d: %q, want %q", tc.name, tc.bass, got, tc.want)
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

func TestToneNameFollowsTheChord(t *testing.T) {
	for _, tc := range []struct {
		chord string
		semis int
		want  string
	}{{"C13", 9, "13"}, {"C6", 9, "6"}, {"C7b13", 8, "b13"}, {"Caug", 8, "#5"}, {"Cm7", 10, "b7"}, {"C9", 2, "9"}} {
		if got := toneName(tc.chord, tc.semis); got != tc.want {
			t.Errorf("%s, %d semitones: %q, want %q", tc.chord, tc.semis, got, tc.want)
		}
	}
}

// The listen screen's way out is the one that works: under Push Hack Home
// Shift + Help opens Home; it said BACK TO LIVE, the 0.2 cycle's.
func TestListenHeadSaysTheWayOut(t *testing.T) {
	draw := func(leave string) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, W, H))
		RenderListen(img, ListenScene{Now: time.Now(), Root: -1, Leave: leave})
		return img.Pix
	}
	if bytes.Equal(draw(""), draw("SHIFT + HELP: PUSH HACK HOME")) {
		t.Fatal("the hint does not follow Leave")
	}
}

// LAST is drawn under the note tiles, dim, and only when there is one.
func TestRenderListenLast(t *testing.T) {
	now := time.Now()
	base := ListenScene{Now: now, Root: -1, Bass: -1, LastRoot: -1, KeysLo: 48, Key: 0, Mask: 0xAB5}
	lit := func(s ListenScene) (n int, bright bool) {
		img := image.NewNRGBA(image.Rect(0, 0, W, H))
		RenderListen(img, s)
		for y := noteY + noteH + 4; y < kbY; y++ {
			for x := noteX; x < W; x++ {
				c := img.NRGBAAt(x, y)
				if c != Ground {
					n++
					if c == Bright || c == Ink {
						bright = true
					}
				}
			}
		}
		return
	}
	if n, _ := lit(base); n != 0 {
		t.Errorf("no LAST, yet %d pixels drawn under the tiles", n)
	}
	s := base
	s.Last, s.LastRoot = "CMAJ", 0
	n, bright := lit(s)
	if n == 0 || bright {
		t.Errorf("LAST CMAJ: %d pixels, bright %v; want some, all dim", n, bright)
	}
	s.Last, s.LastRoot = "BBMAJ", 10 // bVII in C major: the flat glyph, dim
	if n, bright := lit(s); n == 0 || bright {
		t.Errorf("LAST BBMAJ: %d pixels, bright %v", n, bright)
	}
}
