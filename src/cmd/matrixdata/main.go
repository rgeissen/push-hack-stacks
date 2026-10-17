// Command matrixdata exports what the control matrix page shows, from the
// code that runs on Push -- so the page cannot describe a surface that
// does not exist. It writes data.json (each tab's encoder labels, bottom
// row and screen render; each pad page's rows with their LED colours) and
// the pad-map render of each page. scripts/matrix/build.py turns it into
// docs/control-matrix.html; `make matrix` runs both.
//
//	go run ./cmd/matrixdata -manifest upstream/stacks/src/module.json \
//	    -state <captured state .json> -out <dir>
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"

	"github.com/federico-pepe/ableton-push-hack/core/push3"
	"github.com/rgeissen/push-hack-stacks/surface"
)

type knob struct{ Key, Label, Trigger string }

type tab struct {
	Name    string
	Knobs   []knob
	Actions []string
	Screen  string // PNG, base64
}

type row struct {
	Name   string
	Labels []string
	Colors []string // "#rrggbb", "" for a dark pad
}

func main() {
	man := flag.String("manifest", "upstream/stacks/src/module.json", "module.json")
	state := flag.String("state", "", "a captured state map (.json) to draw")
	out := flag.String("out", "matrix", "output folder")
	flag.Parse()

	raw, err := os.ReadFile(*man)
	if err != nil {
		log.Fatal(err)
	}
	m, err := surface.ParseManifest(raw)
	if err != nil {
		log.Fatal(err)
	}
	params := map[string]string{}
	if *state != "" {
		b, err := os.ReadFile(*state)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(b, &params); err != nil {
			log.Fatal(err)
		}
	}
	if params["pat_names"] == "" { // a capture has no slot names: two used slots, named
		params["patterns"] = "0005|1"
		params["pat_names"] = "4:4:A E F#m D||4:4:Am F C G" + "|||||||||||||"
	}
	os.MkdirAll(*out, 0o755)

	names := []string{}
	for _, b := range m.Banks {
		names = append(names, b.Name)
	}
	names = append(names, "PATTERNS", "LIVE")

	var tabs []tab
	for i, n := range names {
		t := tab{Name: n, Knobs: []knob{}, Actions: []string{}}
		if i < len(m.Banks) {
			for _, k := range m.Banks[i].Knobs {
				p := m.Params[k]
				trig := ""
				if p.Trigger {
					trig = "1"
				}
				t.Knobs = append(t.Knobs, knob{k, p.Label(), trig})
			}
		}
		if n != "LIVE" {
			for _, a := range surface.Actions(i) {
				t.Actions = append(t.Actions, a.Label)
			}
			t.Screen = render(surface.Scene{M: m, Params: params, Tab: i, Touched: -1, Held: -1, Scroll: -1, Head: 11.5})
		}
		tabs = append(tabs, t)
	}

	pages := map[string][]row{}
	for pg, pn := range surface.PageNames {
		s := surface.Scene{M: m, Params: params, Held: -1, Scroll: -1}
		g := surface.Pads(s, surface.EditPage(pg))
		rn := surface.RowNames(s, surface.EditPage(pg))
		var rows []row
		for r := 0; r < 8; r++ {
			rw := row{Name: rn[r]}
			for c := 0; c < 8; c++ {
				rw.Labels = append(rw.Labels, g[r][c].Label)
				rw.Colors = append(rw.Colors, hex(g[r][c].Color))
			}
			rows = append(rows, rw)
		}
		pages[pn] = rows
		legend := render(surface.Scene{M: m, Params: params, Tab: 2, Held: -1, Scroll: -1, Legend: true, Page: surface.EditPage(pg)})
		write(filepath.Join(*out, "legend_"+pn+".b64"), []byte(legend))
	}

	j, err := json.MarshalIndent(struct {
		Tabs  []tab
		Pages map[string][]row
	}{tabs, pages}, "", " ")
	if err != nil {
		log.Fatal(err)
	}
	write(filepath.Join(*out, "data.json"), j)
}

func render(sc surface.Scene) string {
	img := image.NewNRGBA(image.Rect(0, 0, surface.W, surface.H))
	surface.Render(img, sc)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func hex(idx uint8) string {
	if idx == 0 {
		return ""
	}
	c := push3.ColorForIndex(idx).RGB
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

func write(path string, b []byte) {
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Fatal(err)
	}
}
