package surface

import (
	"image"
	"image/color"
	"strings"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
)

// drawLibrary is START on Push: every list sits directly under the encoder
// that scrolls it -- genres under GENRE, the genre's progressions under
// PROGRESSION, then COMMON, UNCOMMON and COLOUR -- and the last three
// columns preview what the choice SOUNDS like, as numerals in their
// function colours. Browsing 143 progressions one detent at a time, blind,
// was the 128x64 constraint; this is what the screen is for.
func drawLibrary(img *image.NRGBA, s Scene, p Prog) {
	m := s.M
	var genres []string
	for _, g := range m.Genres {
		if g.Name != "none" {
			genres = append(genres, g.Name)
		}
	}
	drawList(img, 0, genres, s.Params["genre"])

	progs := m.Params["progression"].Options
	for _, g := range m.Genres {
		if g.Name == s.Params["genre"] && g.First+g.Count <= len(progs) {
			progs = progs[g.First : g.First+g.Count]
		}
	}
	drawList(img, 1, progs, s.Params["progression"])
	drawList(img, 2, withoutNone(m.Params["common"].Options), s.Params["common"])
	drawList(img, 3, withoutNone(m.Params["uncommon"].Options), s.Params["uncommon"])
	drawList(img, 4, m.Params["colour"].Options, s.Params["colour"])
	drawPreview(img, 5*ColW, p)
}

const listRow = 14

// drawList draws one column of a list, scrolled so the current entry sits
// in the middle row, the rest fading with distance.
func drawList(img *image.NRGBA, col int, items []string, cur string) {
	x := col * ColW
	if col > 0 {
		gfx.FillRect(img, x, stY0+4, 1, (stY1-stY0)-8, Rule)
	}
	rows := ((stY1 - stY0) - 4) / listRow // 6
	at := -1
	for i, it := range items {
		if it == cur {
			at = i
		}
	}
	top := 0
	if at >= 0 {
		top = at - rows/2
	}
	if top > len(items)-rows {
		top = len(items) - rows
	}
	if top < 0 {
		top = 0
	}
	for r := 0; r < rows && top+r < len(items); r++ {
		i := top + r
		y := stY0 + 2 + r*listRow
		label := text.Truncate(strings.ToUpper(items[i]), 14)
		if i == at {
			gfx.FillRect(img, x+2, y, ColW-4, listRow, Panel)
			gfx.FillRect(img, x+2, y, 2, listRow, Focus)
			text.Draw(img, x+9, y+11, label, Focus)
			continue
		}
		col := Ink
		if d := i - at; at >= 0 && (d > 1 || d < -1) {
			col = Dim
		}
		text.Draw(img, x+9, y+11, label, col)
	}
	if top > 0 {
		gfx.FillRect(img, x+ColW-8, stY0+2, 3, 1, Dim) // more above
	}
	if top+rows < len(items) {
		gfx.FillRect(img, x+ColW-8, stY1-3, 3, 1, Dim) // more below
	}
}

// drawPreview is the progression as harmony: numerals in their function
// colours on one line, the names under them, and how long it is.
func drawPreview(img *image.NRGBA, x int, p Prog) {
	w := 3 * ColW
	gfx.FillRect(img, x, stY0+4, 1, (stY1-stY0)-8, Rule)
	text.Draw(img, x+12, stY0+13, "SOUNDS LIKE", Dim)
	cx := x + 12
	for _, c := range p.Chords {
		roman, acc := Degree(c, p.Key, p.ScaleMask)
		full, _ := FuncHue(FunctionOf(c, p.Key, p.ScaleMask))
		if c.Rest() {
			roman, full = "-", Dim
		}
		cw := text.WidthScaled(roman, 2)
		if acc != 0 {
			cw += 6
		}
		if cx+cw > x+w-12 {
			break
		}
		if acc != 0 {
			drawAccidental(img, cx, stY0+38, acc, full)
			cx += 6
		}
		text.DrawScaled(img, cx, stY0+40, 2, roman, full)
		cx += text.WidthScaled(roman, 2) + 14
	}
	var names []string
	for _, c := range p.Chords {
		names = append(names, DisplayName(c.Name))
	}
	text.Draw(img, x+12, stY0+60, text.Truncate(strings.Join(names, "  "), (w-24)/7), Ink)
	bars := p.ClipUnits / p.StepUnits
	summary := itoa(len(p.Chords)) + " CHORDS  " + itoa(bars) + " BARS"
	text.Draw(img, x+12, stY1-5, summary, Dim)
	keyName := pcNames[p.Key%12]
	right(img, x+w-12, stY1-5, keyName, map[bool]color.NRGBA{true: Ink}[true])
}

func withoutNone(opts []string) []string {
	var out []string
	for _, o := range opts {
		if o != "none" {
			out = append(out, o)
		}
	}
	return out
}
