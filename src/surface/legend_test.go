package surface

import (
	"image"
	"os"
	"testing"
)

// The legend shows the page whose scene button is held, mirrors the pads
// row for row (bottom row at the bottom), and draws nothing of the staff.
func TestLegendMirrorsThePads(t *testing.T) {
	raw, err := os.ReadFile("../upstream/stacks/src/module.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"rhythm": "bossa", "prog": axisProg}
	for page := range PageNames {
		s := Scene{M: m, Params: params, Tab: 2, Held: -1, Scroll: -1, Legend: true, Page: EditPage(page)}
		img := image.NewNRGBA(image.Rect(0, 0, W, H))
		Render(img, s)

		// The held page's tab carries the focus bar; the others do not.
		for i := range PageNames {
			x := lgNameW + i*lgCellW + lgCellW/2
			lit := img.NRGBAAt(x, lgHeadH-1) == Focus
			if lit != (i == page) {
				t.Errorf("page %s held: tab %s focus bar = %v", PageNames[page], PageNames[i], lit)
			}
		}
		// Each cell's left edge is its pad's LED colour, row 0 at the foot.
		g := Pads(s, EditPage(page))
		for row := 0; row < 8; row++ {
			y := lgHeadH + (7-row)*lgRowH + lgRowH/2
			for col := 0; col < 8; col++ {
				p := g[row][col]
				if p.Color == 0 {
					continue
				}
				got := img.NRGBAAt(lgNameW+col*lgCellW+2, y)
				if want := ledRGB(p.Color); got != want {
					t.Fatalf("page %s pad r%d c%d: edge %v, want LED %v", PageNames[page], row, col, got, want)
				}
			}
		}
	}
}
