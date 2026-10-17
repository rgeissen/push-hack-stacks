package surface

import (
	"image"
	"image/color"
	"strings"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
)

// Listen mode: the pads and buttons stay Live's, and the screen names the
// chord being played. Industrial in the split-flap sense -- the name sits
// on flap tiles that flip when it changes, each note is a tile that drops
// in and latches, the keys go down. Motion says what changed; nothing
// moves for its own sake, and at rest the screen is still.
//
//	header     0-14   LISTEN, the track listened to, the key, the way out
//	name      16-108  the chord on flap tiles; its degree and inversion
//	notes     16-108  one tile per note, bass first: interval and name
//	keyboard 112-158  four octaves, the held keys down
const (
	flapW, flapH = 30, 58
	flapScale    = 4
	flapX, flapY = 14, 24
	flapStagger  = 28 * time.Millisecond
	flapFlip     = 170 * time.Millisecond

	noteX, noteY = 500, 30
	noteW, noteH = 58, 54
	noteDrop     = 170 * time.Millisecond
	noteFade     = 150 * time.Millisecond

	kbY, kbH  = 112, 46
	kbOctaves = 4
	keyPress  = 90 * time.Millisecond
)

// The keys are dark, machined: only what is played lights up.
var (
	keyWhite = mix(Dim, Ground, 0.62)
	keyBlack = mix(Panel, Ground, 0.3)
)

// ListenNote is one note played, and when; Off is zero while it is held.
type ListenNote struct {
	Note    int
	On, Off time.Time
}

// ListenScene is everything a listen frame needs.
type ListenScene struct {
	Now      time.Time
	Notes    []ListenNote // held, and released ones still fading
	Name     string       // the chord's name as Stacks spells it ("Cmaj7"), "" none
	PrevName string       // the board's text before (BoardText), for the flap
	NameAt   time.Time    // when Name became the name
	Root     int          // pitch class, -1 none
	Bass     int          // pitch class
	Inv      int
	Known    bool   // a chord is named
	Interval string // two notes: the interval ("MAJOR 3RD"); Name is then the two notes
	Omit     []int  // the chord's notes left out, in semitones above the root
	Key      int    // 0 = C
	Mask     int    // the scale's pitch classes, relative to Key
	KeysLo   int    // the keyboard's lowest note, a C; from KeyWindow
	KeyName  string // "A MINOR": the key, said while nothing is played
	Source   string
	Status   string // what is wrong, if anything: shown instead of the hint
}

// KeyWindow keeps the keyboard still: four octaves from a C, moved by
// whole octaves only when a held note leaves them (the lanes' rule).
func KeyWindow(prev int, notes []ListenNote) int {
	if prev <= 0 || prev%12 != 0 {
		prev = 48 // C3
	}
	lo, hi := 128, -1
	for _, n := range notes {
		if n.Off.IsZero() {
			lo, hi = min(lo, n.Note), max(hi, n.Note)
		}
	}
	if hi < 0 {
		return prev
	}
	top := prev + 12*kbOctaves
	for lo < prev && prev >= 12 {
		prev -= 12
		top -= 12
	}
	for hi >= top && top <= 120 {
		prev += 12
		top += 12
	}
	return prev
}

// RenderListen draws one listen frame. It reports whether anything is
// still moving, so the caller can stop redrawing when nothing is.
func RenderListen(img *image.NRGBA, s ListenScene) (moving bool) {
	gfx.FillRect(img, 0, 0, W, H, Ground)
	drawListenHead(img, s)

	held := heldNotes(s.Notes)
	hue := Ink
	if len(held) > 0 && s.Known && s.Interval == "" && s.Root >= 0 {
		full, _ := FuncHue(listenFunc(s))
		hue = full
	}

	moving = drawFlaps(img, s, hue) || moving
	if len(held) == 0 {
		hint, col := "PLAY A CHORD ON THE PADS", Dim
		if s.KeyName != "" {
			hint = "IN " + s.KeyName + " - " + hint
		}
		if s.Status != "" {
			hint, col = s.Status, Focus
		}
		text.Draw(img, flapX, flapY+flapH+22, text.Truncate(hint, 64), col)
	} else {
		drawChordFacts(img, s, held, hue)
	}
	moving = drawNoteTiles(img, s, hue) || moving
	moving = drawKeyboard(img, s, hue) || moving
	return moving
}

func heldNotes(ns []ListenNote) []int {
	var out []int
	for _, n := range ns {
		if n.Off.IsZero() {
			out = append(out, n.Note)
		}
	}
	return out
}

// listenFunc is the played chord's harmonic role in the key, as the staff
// colours Stacks' own chords.
func listenFunc(s ListenScene) Function {
	if s.Root < 0 {
		return FuncNone
	}
	return FunctionOf(Chord{Root: noteNames[s.Root], Notes: []int{s.Root}}, s.Key, s.Mask)
}

var noteNames = []string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}

func drawListenHead(img *image.NRGBA, s ListenScene) {
	gfx.FillRect(img, 8, 2, 54, 11, Focus)
	center(img, 8, 54, 11, "LISTEN", Ground)
	src := "NO TRACK YET"
	if s.Source != "" {
		src = "FROM " + strings.ToUpper(s.Source)
	}
	text.Draw(img, 72, 11, text.Truncate(src, 40), Ink)
	right(img, W-8, 11, "SHIFT + HELP: BACK TO LIVE", Dim)
	gfx.FillRect(img, 0, 14, W, 1, Rule)
}

// drawFlaps draws the name on flap tiles. When it changes, each tile flips
// from the old character to the new one, left to right: the top half of
// the new character shows as the old top falls, then the new bottom falls
// over the old bottom -- a split-flap board, read at a glance.
func drawFlaps(img *image.NRGBA, s ListenScene, hue color.NRGBA) (moving bool) {
	name, prev := BoardText(s.Name, s.Root, s.Bass, s.Known), s.PrevName
	switch {
	case s.Name == "":
		name = "-"
	case s.Interval != "":
		name = s.Name // the two notes, as they are
	case !s.Known: // nothing fits: the bass alone would read as a chord
		name = "?"
	}
	n := max(len(name), len(prev), 1)
	if done := s.NameAt.Add(time.Duration(n)*flapStagger + flapFlip); !s.Now.Before(done) {
		n = max(len(name), 1) // the flip is over: the board is as long as the name
	}
	for i := 0; i < n; i++ {
		x := flapX + i*(flapW+3)
		if x+flapW > noteX-16 {
			break
		}
		gfx.FillRect(img, x, flapY, flapW, flapH, Panel)
		nc, pc := charAt(name, i), charAt(prev, i)
		col := Bright
		if s.Name == "" {
			col = Dim
		}
		start := s.NameAt.Add(time.Duration(i) * flapStagger)
		p := float64(s.Now.Sub(start)) / float64(flapFlip)
		switch {
		case s.NameAt.IsZero() || p >= 1 || nc == pc:
			flapGlyph(img, x, nc, col, 0, flapH)
		case p < 0:
			flapGlyph(img, x, pc, Ink, 0, flapH)
			moving = true
		case p < 0.5: // new top revealed; the old top falls, squashing
			flapGlyph(img, x, nc, col, 0, flapH/2)
			flapGlyph(img, x, pc, Ink, flapH/2, flapH)
			h := int(float64(flapH/2) * (1 - 2*p))
			flapSquash(img, x, pc, Ink, true, h)
			moving = true
		default: // the new bottom falls over the old bottom
			flapGlyph(img, x, nc, col, 0, flapH/2)
			flapGlyph(img, x, pc, Ink, flapH/2, flapH)
			h := int(float64(flapH/2) * (2*p - 1))
			flapSquash(img, x, nc, col, false, h)
			moving = true
		}
		gfx.FillRect(img, x, flapY+flapH/2, flapW, 1, Ground) // the hinge
	}
	// A rule in the chord's hue under the board: its role, before you read.
	gfx.FillRect(img, flapX, flapY+flapH+3, min(n, 13)*(flapW+3)-3, 2, hue)
	return moving
}

// BoardText is a chord's name as the flap board spells it: Stacks' display
// spelling, and the bass after a slash when it is not the root -- an
// inversion or a bass outside the chord alike.
func BoardText(name string, root, bass int, known bool) string {
	if name == "" {
		return ""
	}
	if !known {
		return "?"
	}
	t := DisplayName(name)
	if bass >= 0 && bass < 12 && bass != root {
		t += "/" + noteNames[bass]
	}
	return t
}

func charAt(s string, i int) string {
	if i < len(s) {
		return s[i : i+1]
	}
	return ""
}

// flapCell renders one character on a tile-sized scratch image.
func flapCell(ch string, col color.NRGBA) *image.NRGBA {
	cell := image.NewNRGBA(image.Rect(0, 0, flapW, flapH))
	if ch != "" {
		w := text.WidthScaled(ch, flapScale)
		text.DrawScaled(cell, (flapW-w)/2, flapH-8, flapScale, ch, col)
	}
	return cell
}

// flapGlyph copies rows y0..y1 of a character's tile onto the board.
func flapGlyph(img *image.NRGBA, x int, ch string, col color.NRGBA, y0, y1 int) {
	cell := flapCell(ch, col)
	for y := y0; y < y1; y++ {
		for cx := 0; cx < flapW; cx++ {
			if c := cell.NRGBAAt(cx, y); c.A > 0 {
				img.SetNRGBA(x+cx, flapY+y, c)
			}
		}
	}
}

// flapSquash draws a falling half-flap h rows tall against the hinge: the
// top half (top=true) hangs above it, the bottom half below it.
func flapSquash(img *image.NRGBA, x int, ch string, col color.NRGBA, top bool, h int) {
	if h <= 0 {
		return
	}
	cell := flapCell(ch, col)
	half := flapH / 2
	for i := 0; i < h; i++ {
		var dy, sy int
		if top {
			dy = half - h + i
			sy = i * half / h
		} else {
			dy = half + i
			sy = half + i*half/h
		}
		gfx.FillRect(img, x, flapY+dy, flapW, 1, mix(Panel, Bright, 0.08))
		for cx := 0; cx < flapW; cx++ {
			if c := cell.NRGBAAt(cx, sy); c.A > 0 {
				img.SetNRGBA(x+cx, flapY+dy, c)
			}
		}
	}
}

// drawChordFacts is the line under the board: degree in the key, its role,
// and the inversion with its bass.
func drawChordFacts(img *image.NRGBA, s ListenScene, held []int, hue color.NRGBA) {
	y := flapY + flapH + 22
	x := flapX
	if s.Interval != "" {
		text.Draw(img, x, y, s.Interval, Ink)
		return
	}
	if s.Known && s.Root >= 0 {
		if roman, acc := Degree(Chord{Root: noteNames[s.Root], Notes: []int{s.Root}}, s.Key, s.Mask); roman != "" {
			if acc != 0 {
				drawAccidental(img, x, y-1, acc, hue)
				x += 6
			}
			text.DrawScaled(img, x, y+2, 2, roman, hue)
			x += text.WidthScaled(roman, 2) + 12
		}
		role := map[Function]string{FuncTonic: "TONIC", FuncPredominant: "PREDOMINANT",
			FuncDominant: "DOMINANT", FuncOutside: "OUTSIDE THE SCALE"}[listenFunc(s)]
		text.Draw(img, x, y, role, Ink)
		x += text.Width(role) + 14
		inv := []string{"ROOT POSITION", "1ST INVERSION", "2ND INVERSION", "3RD INVERSION",
			"4TH INVERSION", "5TH INVERSION"}
		var facts []string
		switch {
		case s.Inv < 0 && s.Bass >= 0:
			facts = append(facts, "BASS "+noteNames[s.Bass])
		case s.Inv < len(inv):
			facts = append(facts, inv[s.Inv])
		}
		for _, o := range s.Omit {
			if n, ok := omitName[o]; ok {
				facts = append(facts, "NO "+n)
			}
		}
		text.Draw(img, x, y, strings.Join(facts, " - "), Dim)
		return
	}
	text.Draw(img, x, y, itoa(len(held))+" NOTES - NO NAMED SHAPE", Dim)
}

// omitName names a note a chord was played without.
var omitName = map[int]string{7: "5TH", 2: "9TH", 5: "11TH", 4: "3RD", 3: "3RD", 10: "7TH", 11: "7TH", 9: "13TH"}

// IntervalWords names two notes' distance, bass up, as the facts line says it.
var IntervalWords = []string{"UNISON", "MINOR 2ND", "MAJOR 2ND", "MINOR 3RD", "MAJOR 3RD", "4TH",
	"TRITONE", "5TH", "MINOR 6TH", "MAJOR 6TH", "MINOR 7TH", "MAJOR 7TH"}

// intervalName is a note's name against the chord's root.
var intervalName = []string{"R", "b9", "9", "b3", "3", "11", "b5", "5", "#5", "6", "b7", "7"}

// drawNoteTiles is one tile per note, bass first: the interval above the
// root, and the note. A new tile drops in from above and latches with a
// bright edge; a released one fades out where it was.
func drawNoteTiles(img *image.NRGBA, s ListenScene, hue color.NRGBA) (moving bool) {
	notes := append([]ListenNote(nil), s.Notes...)
	for i := 1; i < len(notes); i++ { // by pitch, bass first
		for j := i; j > 0 && notes[j-1].Note > notes[j].Note; j-- {
			notes[j-1], notes[j] = notes[j], notes[j-1]
		}
	}
	ref := s.Root
	if !s.Known || s.Interval != "" || ref < 0 {
		ref = s.Bass
	}
	for i, n := range notes {
		if i >= 7 {
			break
		}
		x := noteX + i*(noteW+6)
		y := noteY
		fade := 0.0
		if !n.Off.IsZero() {
			fade = float64(s.Now.Sub(n.Off)) / float64(noteFade)
			if fade >= 1 {
				continue
			}
			moving = true
		}
		drop := float64(s.Now.Sub(n.On)) / float64(noteDrop)
		latch := false
		if drop < 1 {
			e := 1 - (1-drop)*(1-drop)*(1-drop) // ease out
			y -= int((1 - e) * 26)
			moving = true
		} else if drop < 1.6 {
			latch = true
			moving = true
		}
		edge, ink := hue, Bright
		if ref >= 0 && ((n.Note%12-ref)+12)%12 == 0 {
			edge = Focus // the root: amber, as focus is everywhere else
		}
		if fade > 0 {
			edge, ink = mix(edge, Ground, fade), mix(ink, Ground, fade)
		}
		bg := mix(edge, Ground, 0.82)
		gfx.FillRect(img, x, y, noteW, noteH, bg)
		gfx.FillRect(img, x, y, noteW, 3, edge)
		if latch {
			frame(img, x, y, noteW, noteH, mix(edge, Bright, 0.5))
		}
		iv := "?"
		if ref >= 0 {
			iv = toneName(s.Name, ((n.Note%12-ref)+12)%12)
		}
		drawInterval(img, x, y+32, iv, ink)
		center(img, x, noteW, y+noteH-6, noteNames[n.Note%12]+itoa(n.Note/12-2), mix(ink, bg, 0.3))
	}
	return moving
}

// drawKeyboard is four octaves from s.KeysLo; a held key goes down -- it
// fills from the top in the chord's hue, the root amber -- and comes back
// up when released.
func drawKeyboard(img *image.NRGBA, s ListenScene, hue color.NRGBA) (moving bool) {
	whites := 7 * kbOctaves
	ww := (W - 8) / whites
	x0 := (W - ww*whites) / 2
	state := map[int]ListenNote{}
	for _, n := range s.Notes {
		state[n.Note] = n
	}
	ref := s.Root
	if !s.Known || s.Interval != "" || ref < 0 {
		ref = s.Bass
	}
	keyCol := func(note int) color.NRGBA {
		if ref >= 0 && ((note%12-ref)+12)%12 == 0 {
			return Focus
		}
		return hue
	}
	// fill reports how far down a key is (0..1) and whether it moves.
	fill := func(note int) (float64, bool) {
		n, ok := state[note]
		if !ok {
			return 0, false
		}
		if !n.Off.IsZero() {
			f := 1 - float64(s.Now.Sub(n.Off))/float64(noteFade)
			return max(f, 0), f > 0
		}
		f := float64(s.Now.Sub(n.On)) / float64(keyPress)
		return min(f, 1), f < 1
	}
	whitePC := []int{0, 2, 4, 5, 7, 9, 11}
	for i := 0; i < whites; i++ {
		note := s.KeysLo + 12*(i/7) + whitePC[i%7]
		x := x0 + i*ww
		gfx.FillRect(img, x+1, kbY, ww-2, kbH, keyWhite)
		if f, mv := fill(note); f > 0 {
			gfx.FillRect(img, x+1, kbY, ww-2, int(float64(kbH)*f), keyCol(note))
			moving = moving || mv
		}
		if note%12 == 0 { // the C's octave, small, at the foot
			center(img, x, ww, kbY+kbH-4, "C"+itoa(note/12-2), Dim)
		}
	}
	blackAfter := map[int]int{0: 1, 1: 3, 3: 6, 4: 8, 5: 10} // white index in octave -> black pc
	bw, bh := ww*6/10, kbH*6/10
	for i := 0; i < whites; i++ {
		pc, ok := blackAfter[i%7]
		if !ok {
			continue
		}
		note := s.KeysLo + 12*(i/7) + pc
		x := x0 + (i+1)*ww - bw/2
		gfx.FillRect(img, x-1, kbY, bw+2, bh+1, Ground) // the gap around it
		gfx.FillRect(img, x, kbY, bw, bh, keyBlack)
		if f, mv := fill(note); f > 0 {
			gfx.FillRect(img, x+1, kbY, bw-2, int(float64(bh-1)*f), keyCol(note))
			moving = moving || mv
		}
	}
	return moving
}

// drawInterval centres an interval label at 2x in a tile: "b9" and "#11"
// get the staff's drawn flat and sharp -- the face has no flat, and a
// "b" reads as the note B.
func drawInterval(img *image.NRGBA, x, baseline int, iv string, col color.NRGBA) {
	acc := 0
	switch {
	case strings.HasPrefix(iv, "b"):
		acc, iv = -1, iv[1:]
	case strings.HasPrefix(iv, "#"):
		acc, iv = 1, iv[1:]
	}
	w := text.WidthScaled(iv, 2)
	if acc != 0 {
		w += 13
	}
	dx := x + (noteW-w)/2
	if acc != 0 {
		drawAccidentalK(img, dx, baseline-1, acc, col, 2)
		dx += 13
	}
	text.DrawScaled(img, dx, baseline, 2, iv, col)
}

// NoteName is a MIDI note's name without its octave: 61 is "C#".
func NoteName(n int) string { return noteNames[((n%12)+12)%12] }

// toneName is a note's place in the chord, spelled as the chord's name
// spells it: in a 13th chord the major 6th is the 13, and a minor 6th the
// b13 of a 7b13; anywhere else they are 6 and #5.
func toneName(chord string, semis int) string {
	switch {
	case semis == 9 && strings.Contains(chord, "13"):
		return "13"
	case semis == 8 && strings.Contains(chord, "b13"):
		return "b13"
	}
	return intervalName[semis]
}
