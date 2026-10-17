// Command matrixdata exports what the control matrix page shows, from the
// code that runs on Push -- so the page cannot describe a surface that
// does not exist. It writes data.json (each tab's encoder labels, bottom
// row and screen render; each pad page's rows with their LED colours) and
// the pad-map render of each page. scripts/matrix/build.py turns it into
// docs/control-matrix.html; `make matrix` runs both.
//
// The state is the real core's: the progression loaded and chord 1
// selected inside Stacks itself, every value read back -- so the CHORD
// tab's encoders show the chord the frame selects. The pictures show it
// playing, with the playhead inside chord 1.
//
//	go run ./cmd/matrixdata -manifest upstream/stacks/src/module.json \
//	    -progression Axis -out <dir>
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
	"strings"

	"github.com/federico-pepe/ableton-push-hack/core/push3"
	"github.com/rgeissen/push-hack-stacks/dsp"
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
	progression := flag.String("progression", "Axis", "the library progression to draw")
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
	params := coreState(m, *progression)
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
			t.Screen = render(surface.Scene{M: m, Params: params, Tab: i, Touched: -1, Held: -1, Scroll: -1, Head: 3.5})
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

// coreState runs Stacks: the progression loaded, kept in pattern slot 1
// and copied to slot 3 (so PATTERNS has slots to show), chord 1 selected;
// then every param the manifest names, read back. The frame is drawn
// playing: Running on and chord 1 sounding, in `prog`'s header.
func coreState(m *surface.Manifest, progression string) map[string]string {
	if err := dsp.Init(); err != nil {
		log.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "matrixdata")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	in, err := dsp.New(dir, `{"state_path":"`+filepath.Join(dir, "state.json")+`"}`)
	if err != nil {
		log.Fatal(err)
	}
	in.SetParam("progression", progression)
	in.SetParam("pat_store", "3")
	in.SetParam("sel", "1")
	params := map[string]string{}
	for k := range m.Params {
		if v, ok := in.GetParam(k); ok {
			params[k] = v
		}
	}
	for _, k := range []string{"prog", "patterns", "pat_names", "status"} {
		params[k], _ = in.GetParam(k)
	}
	if params["progression"] != progression {
		log.Fatalf("progression %q did not load (got %q)", progression, params["progression"])
	}
	h := strings.Split(params["prog"], "|")
	if len(h) < 14 || h[2] != "1" {
		log.Fatalf("prog: chord 1 not selected: %.40q", params["prog"])
	}
	h[3], h[8] = "0", "1" // chord 1 sounding, transport running
	params["prog"] = strings.Join(h, "|")
	return params
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
