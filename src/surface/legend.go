package surface

import (
	"image"
	"image/color"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

// The legend is the pad grid drawn on the screen while a scene button is
// held: what every row of the page you are picking does, and what each pad
// says, laid out as the pads are -- bottom row at the bottom. It takes the
// whole screen because it is a question being answered ("what is under my
// fingers?"), and it goes the moment the button is let go.
const (
	lgHeadH = 16
	lgRowH  = 18
	lgNameW = 136
	lgCellW = (W - lgNameW) / 8
)

func drawLegend(img *image.NRGBA, s Scene) {
	gfx.FillRect(img, 0, 0, W, H, Ground)

	// Head: the three pages, the held one lit, as the tabs are.
	text.Draw(img, 8, 12, "PADS", Dim)
	for i, name := range PageNames {
		x := lgNameW + i*lgCellW
		col := Dim
		if EditPage(i) == s.Page {
			col = Bright
			gfx.FillRect(img, x+8, lgHeadH-2, lgCellW-16, 2, Focus)
		}
		center(img, x, lgCellW, 12, name, col)
	}

	g := Pads(s, s.Page)
	names := RowNames(s, s.Page)
	rows := editRows(s.Page, s.M, s.Params, s.Scroll)
	for row := 0; row < 8; row++ {
		y := lgHeadH + (7-row)*lgRowH
		nameCol := Ink
		name := names[row]
		switch {
		case row >= 2 && row <= 5:
			nameCol = named(rowHue[row-2])
			if er := rows[row-2]; er.Total > 8 { // the long row: which eight of how many
				name += " " + itoa(er.Start+1) + "-" + itoa(er.Start+len(er.Options)) + "/" + itoa(er.Total)
			}
		case row >= 6:
			nameCol = Dim
		}
		text.Draw(img, 8, y+13, text.Truncate(name, (lgNameW-12)/7), nameCol)
		for col := 0; col < 8; col++ {
			drawLegendCell(img, lgNameW+col*lgCellW, y, g[row][col])
		}
	}
	// The three groups: chords, the page's editor rows, patterns.
	for _, row := range []int{2, 6} {
		gfx.FillRect(img, 0, lgHeadH+(8-row)*lgRowH-1, W, 1, Rule)
	}
}

// drawLegendCell is one pad: its LED colour, softened so the label reads,
// and the value it holds now in focus amber, exactly as the pad lights.
func drawLegendCell(img *image.NRGBA, x, y int, p Pad) {
	if p.Color == 0 { // dark pad: an empty slot keeps its number, dim
		gfx.FillRect(img, x+2, y+2, lgCellW-4, lgRowH-3, Panel)
		if p.Label != "" {
			center(img, x+2, lgCellW-4, y+14, p.Label, Dim)
		}
		return
	}
	led := ledRGB(p.Color)
	fill, ink := mix(led, Ground, 0.6), Bright
	if led == Focus {
		fill, ink = Focus, Ground
	}
	gfx.FillRect(img, x+2, y+2, lgCellW-4, lgRowH-3, fill)
	gfx.FillRect(img, x+2, y+2, 2, lgRowH-3, led) // the LED's own hue, unsoftened
	if p.Label != "" {
		center(img, x+2, lgCellW-4, y+14, text.Truncate(p.Label, (lgCellW-10)/7), ink)
	}
}

// ledRGB is a pad's palette index as the screen draws it.
func ledRGB(idx uint8) color.NRGBA { return push3.ColorForIndex(idx).RGB }
