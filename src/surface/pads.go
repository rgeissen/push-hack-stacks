package surface

import (
	"strconv"

	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

// The 8x8 pads, bottom row first -- the row your hand rests on.
//
//	rows 1-2  the chords, 1-8 and 9-16. Function colour; the selected chord
//	          amber; the sounding chord white. Never paged, never repurposed:
//	          the row your hand can trust (Stacks' rule on Move).
//	rows 3-6  four editor rows for the selected chord, one parameter per
//	          row. Three pages -- WRITE, VOICE, FEEL -- on the scene buttons.
//	          Each row keeps ONE hue by position, so the colour says WHERE
//	          you are and amber says which value is set.
//	rows 7-8  the sixteen pattern slots, always there.

// Pad is one pad's LED (a Push palette index) and what it does, for the
// control matrix and the mockups.
type Pad struct {
	Color uint8
	Label string
	Key   string // the param a press writes (editor rows); "" for none
	Value string // the value it writes
}

// EditPage is which set of editor rows is up.
type EditPage int

const (
	PageWrite EditPage = iota
	PageVoice
	PageFeel
)

var PageNames = []string{"WRITE", "VOICE", "FEEL"}

// EditRow is one editor row: the param it writes and its eight choices.
type EditRow struct {
	Key     string
	Name    string
	Options []string // what each pad writes
	Labels  []string // what each pad is called
	Start   int      // first option shown, for a row longer than eight
	Total   int      // options in the whole row
}

// editRows returns the four rows of a page for the selected chord.
func editRows(page EditPage, m *Manifest, params map[string]string, scroll int) []EditRow {
	switch page {
	case PageVoice:
		return []EditRow{
			{Key: "inv", Name: "INVERSION", Options: []string{"-3", "-2", "-1", "0", "1", "2", "3"},
				Labels: []string{"-3", "-2", "-1", "0", "+1", "+2", "+3"}},
			window("ccolour", "COLOUR", m.Params["ccolour"].Options, params["ccolour"], scroll),
			{Key: "coct", Name: "OCTAVE", Options: []string{"1", "2", "3", "4", "5"}, Labels: []string{"1", "2", "3", "4", "5"}},
			{Key: "cstrum", Name: "STRUM", Options: []string{"0", "10", "20", "35", "50", "65", "80", "100"},
				Labels: []string{"0", "10", "20", "35", "50", "65", "80", "100"}},
		}
	case PageFeel:
		return []EditRow{
			window("rhythm", "RHYTHM", m.Params["rhythm"].Options, params["rhythm"], scroll),
			{Key: "cgate", Name: "GATE", Options: []string{"-50", "-35", "-20", "-10", "0", "10", "25", "50"},
				Labels: []string{"-50", "-35", "-20", "-10", "0", "+10", "+25", "+50"}},
			{Key: "cvel", Name: "VELOCITY", Options: []string{"-63", "-40", "-20", "-10", "0", "10", "20", "40"},
				Labels: []string{"-63", "-40", "-20", "-10", "0", "+10", "+20", "+40"}},
			{Key: "off", Name: "OFFSET", Options: []string{"0", "1", "2", "3", "4", "6", "8", "12"},
				Labels: []string{"0", "1", "2", "3", "4", "6", "8", "12"}},
		}
	}
	shapes := familyShapes(m, params["family"])
	return []EditRow{
		{Key: "degree", Name: "DEGREE", Options: []string{"I", "II", "III", "IV", "V", "VI", "VII", "rest"},
			Labels: []string{"I", "II", "III", "IV", "V", "VI", "VII", "REST"}},
		{Key: "len", Name: "LENGTH", Options: []string{"2", "4", "6", "8", "16", "24", "32", "64"},
			Labels: []string{"1/4", "1/2", "3/4", "1", "2", "3", "4", "8"}},
		{Key: "family", Name: "FAMILY", Options: m.Params["family"].Options, Labels: upper(m.Params["family"].Options)},
		window("shape", "SHAPE", shapes, params["shape"], scroll),
	}
}

// window is a list longer than eight. scroll < 0 keeps the current value on
// the row; Page Left/Right set scroll to show another eight (the page has
// at most one such row).
func window(key, name string, opts []string, cur string, scroll int) EditRow {
	at := 0
	for i, o := range opts {
		if o == cur {
			at = i
		}
	}
	start := 0
	if len(opts) > 8 {
		start = min(max(at-3, 0), len(opts)-8)
		if scroll >= 0 {
			start = min(scroll, len(opts)-8)
		}
	}
	end := min(start+8, len(opts))
	return EditRow{Key: key, Name: name, Options: opts[start:end], Labels: upper(opts[start:end]),
		Start: start, Total: len(opts)}
}

// LongRow finds the page's row of more than eight options: its pad row
// (2-5), where it starts, and how many there are. ok is false when the
// page has none (Page Left/Right then have nothing to scroll).
func LongRow(s Scene, page EditPage) (row, start, total int, ok bool) {
	for r, er := range editRows(page, s.M, s.Params, s.Scroll) {
		if er.Total > 8 {
			return 2 + r, er.Start, er.Total, true
		}
	}
	return 0, 0, 0, false
}

// familyShapes is the run of Chord Shape options that belongs to a family
// (module.json's shape_families, read as the options' order).
func familyShapes(m *Manifest, family string) []string {
	shapes := m.Params["shape"].Options
	runs := map[string][2]int{"5th": {0, 4}, "triad": {4, 6}, "6th": {10, 3}, "7th": {13, 11}, "9th": {24, 8}, "ext": {32, 6}}
	r, ok := runs[family]
	if !ok || r[0]+r[1] > len(shapes) {
		return nil
	}
	return shapes[r[0] : r[0]+r[1]]
}

// rowHue is an editor row's colour, by position: hues no chord function
// and no focus uses, so a lit editor pad can never be read as a chord.
var rowHue = []string{"cyan_lt", "lavender", "sage", "pink"}

// Pads computes all 64 pads. pads[row][col], row 0 at the bottom.
func Pads(s Scene, page EditPage) [8][8]Pad {
	var g [8][8]Pad
	idx := func(name string) uint8 { i, _ := push3.ColorByName(name); return i }
	off, focus, white := idx("off"), idx("amber"), idx("white_btn")

	p, ok := ParseProg(s.Params["prog"])
	p.Playing = p.Sounding(s.Held)
	for i := 0; i < 16; i++ {
		row, col := i/8, i%8
		g[row][col] = Pad{Color: off}
		if !ok || i >= len(p.Chords) {
			continue
		}
		c := p.Chords[i]
		_, rest := FuncPad(FunctionOf(c, p.Key, p.ScaleMask))
		full, _ := FuncPad(FunctionOf(c, p.Key, p.ScaleMask))
		color := full
		if c.Mute {
			color = rest
		}
		if c.Rest() {
			color = idx("gray_blue")
		}
		if i+1 == p.Sel {
			color = focus
		}
		if i == p.Playing {
			color = white
		}
		g[row][col] = Pad{Color: color, Label: DisplayName(c.Name), Value: strconv.Itoa(i + 1)}
	}

	cur := map[string]string{}
	for k, v := range s.Params {
		cur[k] = v
	}
	if ok && p.Sel >= 1 && p.Sel <= len(p.Chords) { // degree is write-only: read it off the chord
		if r, acc := Degree(p.Chords[p.Sel-1], p.Key, p.ScaleMask); acc == 0 {
			cur["degree"] = r
		}
		if p.Chords[p.Sel-1].Rest() {
			cur["degree"] = "rest"
		}
	}
	for r, er := range editRows(page, s.M, s.Params, s.Scroll) {
		hue := idx(rowHue[r])
		for col := 0; col < 8; col++ {
			pad := Pad{Color: off}
			if col < len(er.Options) {
				pad = Pad{Color: hue, Label: er.Labels[col], Key: er.Key, Value: er.Options[col]}
				if er.Options[col] == cur[er.Key] {
					pad.Color = focus
				}
			}
			g[2+r][col] = pad
		}
	}

	mask, active := parsePatterns(s.Params["patterns"])
	for i := 0; i < 16; i++ {
		row, col := 6+i/8, i%8
		pad := Pad{Color: off, Label: pad2(i + 1)}
		if mask&(1<<i) != 0 {
			pad.Color = idx("steel")
		}
		if i+1 == active {
			pad.Color = focus
		}
		g[row][col] = pad
	}
	return g
}

// RowNames labels the eight rows, bottom first, for a page.
func RowNames(s Scene, page EditPage) [8]string {
	var n [8]string
	n[0], n[1] = "CHORDS 1-8", "CHORDS 9-16"
	for r, er := range editRows(page, s.M, s.Params, s.Scroll) {
		n[2+r] = er.Name
	}
	n[6], n[7] = "PATTERNS 1-8", "PATTERNS 9-16"
	return n
}

func upper(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		b := []byte(s)
		for j, c := range b {
			if c >= 'a' && c <= 'z' {
				b[j] = c - 32
			}
		}
		out[i] = string(b)
	}
	return out
}
