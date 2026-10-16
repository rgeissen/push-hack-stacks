package surface

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Param is one declared control, from module.json's chain_params.
type Param struct {
	Key, Name, Short string
	Type             string // "int" or "enum"
	Min, Max         int
	Options          []string
	Trigger          bool // access "write": fires, has no value of its own
}

// Bank is one knob page. Its 8 slots are the encoders, left to right, in
// EXACTLY the Move order, so a hand that learned Stacks on Move is at home
// here ("the banks are the levels").
type Bank struct {
	ID, Name string
	Knobs    []string // param keys; "" is an empty slot
}

// Genre is one run of the progression library.
type Genre struct {
	Name         string
	First, Count int
}

// Manifest is what module.json declares, read once.
type Manifest struct {
	Params map[string]Param
	Banks  []Bank
	Genres []Genre
}

// bankOrder is Push's tab order. MAIN is Move's root level less `sel`,
// which the jog owns on both devices.
var bankOrder = []struct{ id, name string }{
	{"root", "MAIN"}, {"start", "START"}, {"chord", "CHORD"},
	{"voice", "VOICE"}, {"feel", "FEEL"}, {"clip", "CLIP"},
}

func ParseManifest(data []byte) (*Manifest, error) {
	var raw struct {
		Capabilities struct {
			ChainParams []struct {
				Key       string          `json:"key"`
				Name      string          `json:"name"`
				ShortName string          `json:"short_name"`
				Type      string          `json:"type"`
				Min       json.Number     `json:"min"`
				Max       json.Number     `json:"max"`
				Options   []string        `json:"options"`
				Access    string          `json:"access"`
				Default   json.RawMessage `json:"default"`
			} `json:"chain_params"`
			GenreProgressions []struct {
				Name  string `json:"name"`
				First int    `json:"first"`
				Count int    `json:"count"`
			} `json:"genre_progressions"`
		} `json:"capabilities"`
		UI struct {
			Levels map[string]struct {
				Knobs []string `json:"knobs"`
			} `json:"levels"`
		} `json:"ui_hierarchy"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	m := &Manifest{Params: map[string]Param{}}
	for _, p := range raw.Capabilities.ChainParams {
		mn, _ := strconv.Atoi(p.Min.String())
		mx, _ := strconv.Atoi(p.Max.String())
		typ := p.Type
		if len(p.Options) > 0 {
			typ = "enum"
		}
		m.Params[p.Key] = Param{Key: p.Key, Name: p.Name, Short: p.ShortName, Type: typ,
			Min: mn, Max: mx, Options: p.Options, Trigger: p.Access == "write"}
	}
	for _, b := range bankOrder {
		lv, ok := raw.UI.Levels[b.id]
		if !ok {
			continue
		}
		var knobs []string
		for _, k := range lv.Knobs {
			if k != "sel" {
				knobs = append(knobs, k)
			}
		}
		for len(knobs) < 8 {
			knobs = append(knobs, "")
		}
		m.Banks = append(m.Banks, Bank{ID: b.id, Name: b.name, Knobs: knobs[:8]})
	}
	for _, g := range raw.Capabilities.GenreProgressions {
		m.Genres = append(m.Genres, Genre{g.Name, g.First, g.Count})
	}
	return m, nil
}

// Label is the name a 120 px cell can afford: the full name, uppercase,
// when it fits in 16 glyphs, else the short name.
func (p Param) Label() string {
	if n := strings.ToUpper(p.Name); len(n) <= 16 {
		return n
	}
	return strings.ToUpper(p.Short)
}

// Frac places a value on its range, 0..1, for the gauge. ok is false for a
// trigger or a value the declaration does not know.
func (p Param) Frac(val string) (float64, bool) {
	if p.Trigger {
		return 0, false
	}
	if p.Type == "enum" {
		if len(p.Options) < 2 {
			return 0, false
		}
		for i, o := range p.Options {
			if o == val {
				return float64(i) / float64(len(p.Options)-1), true
			}
		}
		return 0, false
	}
	v, err := strconv.Atoi(val)
	if err != nil || p.Max <= p.Min {
		return 0, false
	}
	f := float64(v-p.Min) / float64(p.Max-p.Min)
	return clamp01(f), true
}

// Bipolar reports a range centred on zero (Inversion, Transpose, offsets),
// which the gauge draws outward from the middle.
func (p Param) Bipolar() bool { return p.Type == "int" && p.Min < 0 && p.Max > 0 }

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
