// Command mockup renders the Stacks surface from captured device states,
// through the same renderer the device uses -- so a mockup is a picture of
// what ships, not of what we hope ships.
//
//	go run ./cmd/mockup -manifest upstream/stacks/src/module.json \
//	    -data <dir of captured state .json> -out <dir>
package main

import (
	"encoding/json"
	"flag"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
	"github.com/rgeissen/push-hack-stacks/surface"
)

type scene struct {
	name, data string
	tab        int
	touched    int
	sel        int     // 0 keeps the captured selection
	playing    int     // -1 none
	head       float64 // playhead, units
	toast      string
	pads       int // edit page to render the pads for, -1 none
}

var scenes = []scene{
	{name: "01-chord-axis", data: "axis", tab: 2, touched: -1, sel: 2, playing: 2, head: 19.5, pads: 0},
	{name: "02-voice-neosoul", data: "neosoul", tab: 3, touched: 4, playing: 0, head: 3, pads: 1},
	{name: "03-main-blues", data: "blues", tab: 0, touched: -1, sel: 5, playing: 4, head: 52, pads: -1},
	{name: "04-feel-afterlife", data: "afterlife", tab: 4, touched: 3, sel: 1, playing: 1, head: 11, pads: 2},
	{name: "05-chord-folia12", data: "folia12", tab: 2, touched: -1, sel: 10, playing: 9, head: 74, pads: -1},
	{name: "06-start-ipanema", data: "ipanema", tab: 1, touched: 1, sel: 2, playing: -1, pads: -1},
	{name: "07-patterns", data: "axis", tab: surface.TabPatterns, touched: -1, sel: 1, playing: -1, pads: -1},
	{name: "08-clip-armed", data: "bird", tab: 5, touched: 2, sel: 4, playing: -1, toast: "ARMED - PRESS RECORD ON PUSH", pads: -1},
}

func main() {
	man := flag.String("manifest", "upstream/stacks/src/module.json", "module.json")
	data := flag.String("data", "", "folder of captured state maps (<name>.json)")
	out := flag.String("out", "mockups", "output folder")
	scale := flag.Int("scale", 2, "pixel scale of the review copy")
	flag.Parse()

	raw, err := os.ReadFile(*man)
	if err != nil {
		log.Fatal(err)
	}
	m, err := surface.ParseManifest(raw)
	if err != nil {
		log.Fatal(err)
	}
	os.MkdirAll(*out, 0o755)
	for _, sc := range scenes {
		params := map[string]string{}
		b, err := os.ReadFile(filepath.Join(*data, sc.data+".json"))
		if err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(b, &params); err != nil {
			log.Fatal(err)
		}
		if sc.name == "07-patterns" {
			params["patterns"] = "0a65|3" // slots 1,3,6,7,9,12 used; in slot 3
		}
		params["prog"] = setHeader(params["prog"], sc)
		img := image.NewNRGBA(image.Rect(0, 0, surface.W, surface.H))
		surface.Render(img, surface.Scene{M: m, Params: params, Tab: sc.tab, Touched: sc.touched, Held: -1, Scroll: -1,
			Head: sc.head, Toast: sc.toast})
		write(filepath.Join(*out, sc.name+".png"), img)
		write(filepath.Join(*out, sc.name+"@"+itoa(*scale)+"x.png"), upscale(img, *scale))
		if sc.pads >= 0 {
			s := surface.Scene{M: m, Params: params, Tab: sc.tab, Held: -1, Scroll: -1}
			write(filepath.Join(*out, sc.name+"-pads.png"), renderPads(s, surface.EditPage(sc.pads)))
			s.Legend, s.Page = true, surface.EditPage(sc.pads) // the scene button held
			surface.Render(img, s)
			write(filepath.Join(*out, sc.name+"-legend@"+itoa(*scale)+"x.png"), upscale(img, *scale))
		}
		log.Printf("%s", sc.name)
	}
}

// renderPads draws the 8x8 grid as the hardware shows it -- LED colours
// from Push's palette -- with each pad's meaning printed on it and the row
// names beside it, plus the scene buttons that pick the editor page.
func renderPads(s surface.Scene, page surface.EditPage) *image.NRGBA {
	const cell, gap, left, top = 58, 6, 132, 30
	g := surface.Pads(s, page)
	names := surface.RowNames(s, page)
	w, h := left+8*(cell+gap)+90, top+8*(cell+gap)+10
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	gfx.FillRect(img, 0, 0, w, h, color.NRGBA{12, 14, 16, 255})
	text.Draw(img, left, 20, "PADS - EDIT PAGE "+surface.PageNames[page], surface.Ink)
	for row := 0; row < 8; row++ {
		y := top + (7-row)*(cell+gap)
		text.Draw(img, 10, y+cell/2+5, names[row], surface.Dim)
		for col := 0; col < 8; col++ {
			x := left + col*(cell+gap)
			pad := g[row][col]
			c := push3.ColorForIndex(pad.Color).RGB
			if pad.Color == 0 {
				c = color.NRGBA{30, 33, 37, 255}
			}
			gfx.FillRect(img, x, y, cell, cell, c)
			lum := int(c.R)*3 + int(c.G)*6 + int(c.B)
			ink := color.NRGBA{10, 10, 10, 255}
			if lum < 900 {
				ink = color.NRGBA{235, 238, 240, 255}
			}
			if pad.Label != "" {
				l := text.Truncate(pad.Label, 7)
				text.Draw(img, x+(cell-text.Width(l))/2, y+cell/2+5, l, ink)
			}
		}
	}
	sx := left + 8*(cell+gap) + 14
	for i, name := range surface.PageNames {
		y := top + i*(cell+gap)
		c := color.NRGBA{60, 64, 70, 255}
		ink := surface.Ink
		if surface.EditPage(i) == page {
			c, ink = surface.Focus, color.NRGBA{10, 10, 10, 255}
		}
		gfx.FillRect(img, sx, y+cell/2-10, 64, 20, c)
		text.Draw(img, sx+(64-text.Width(name))/2, y+cell/2+5, name, ink)
	}
	return img
}

// setHeader sets the selected chord, the sounding chord and Running in a
// captured `prog`, so one capture can show a scene mid-playback.
func setHeader(prog string, sc scene) string {
	h := strings.Split(prog, "|")
	if len(h) < 14 {
		return prog
	}
	if sc.sel > 0 {
		h[2] = itoa(sc.sel)
	}
	h[3] = itoa(sc.playing)
	if sc.playing >= 0 {
		h[8] = "1"
	}
	return strings.Join(h, "|")
}

func upscale(src *image.NRGBA, k int) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx()*k, b.Dy()*k))
	for y := 0; y < b.Dy()*k; y++ {
		for x := 0; x < b.Dx()*k; x++ {
			dst.Set(x, y, src.At(x/k, y/k))
		}
	}
	return dst
}

func write(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
