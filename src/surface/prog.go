// Package surface draws Stacks on Push 3: the 960x160 screen and the 8x8
// pads, from what the module publishes (`prog`, `status`, `patterns` and
// the param map). It reads nothing itself and keeps no state of its own
// beyond what a Scene hands it, so a mockup and the live device draw the
// same picture from the same data.
package surface

import (
	"strconv"
	"strings"
)

// Chord is one slot of the progression, as `prog` publishes it.
type Chord struct {
	Name  string // Stacks' own spelling: "F#m7", "Amaj7"
	Root  string // "A3"
	Inv   int
	Len   int // eighths of a bar, absolute
	Off   int
	Vel   int
	Mute  bool
	Mask  int   // rhythm figure: bit b = a hit at sixteenth b of the bar
	Notes []int // resolved MIDI notes, as they sound
}

// Rest reports a slot that sounds nothing.
func (c Chord) Rest() bool { return len(c.Notes) == 0 }

// Prog is the whole progression in one read -- the module's own rule
// ("the picture costs ONE read").
type Prog struct {
	Sel       int // 1-based selected chord
	Playing   int // 0-based sounding chord, -1 none
	StepUnits int // units per bar (8: a unit is an eighth of a bar)
	ScaleMask int // pitch classes of the scale, relative to Key
	Key       int // 0 = C
	ClipUnits int
	Running   bool
	BPM       int
	PosUnits  int
	Chords    []Chord
}

// ParseProg reads `prog` (v12 or v13). A read that does not parse returns
// ok=false and must draw nothing -- the module's rule that a missing read
// never becomes a plausible picture.
func ParseProg(s string) (Prog, bool) {
	h := strings.Split(s, "|")
	if len(h) < 14 || (h[0] != "v13" && h[0] != "v12") {
		return Prog{}, false
	}
	n := func(i, dflt int) int {
		v, err := strconv.Atoi(h[i])
		if err != nil {
			return dflt
		}
		return v
	}
	mask, _ := strconv.ParseInt(h[5], 16, 32)
	p := Prog{
		Sel: n(2, 1), Playing: n(3, -1), StepUnits: n(4, 8), ScaleMask: int(mask),
		Key: n(6, 0), ClipUnits: n(7, 32), Running: h[8] == "1", BPM: n(10, 120),
		PosUnits: n(11, 0),
	}
	if p.StepUnits < 1 {
		p.StepUnits = 8
	}
	for _, part := range strings.Split(strings.Join(h[13:], "|"), ";") {
		f := strings.Split(part, ",")
		if len(f) < 9 {
			continue
		}
		c := Chord{Name: f[0], Root: f[1]}
		c.Inv, _ = strconv.Atoi(f[2])
		c.Len, _ = strconv.Atoi(f[3])
		c.Off, _ = strconv.Atoi(f[4])
		c.Vel, _ = strconv.Atoi(f[5])
		c.Mute = f[6] == "1"
		// The rhythm mask is DECIMAL (stacks.c fmt_prog prints it with %d),
		// unlike the header's scale mask, which is hex. Read as hex it drew a
		// different figure than the one played -- caught on device: "quarter"
		// arrives as 4369 = 0x1111 and plays on units 0, 2, 4, 6.
		m, _ := strconv.Atoi(f[7])
		c.Mask = m
		if c.Len < 1 {
			c.Len = 1
		}
		if c.Mask == 0 {
			c.Mask = 1
		}
		// An empty note list is a rest, not a note at pitch 0 (web_ui.html
		// learned this the hard way).
		for _, t := range strings.Split(f[8], ".") {
			if t == "" {
				continue
			}
			note, _, _ := strings.Cut(t, ":")
			v, err := strconv.Atoi(note)
			if err == nil && v >= 0 && v <= 127 {
				c.Notes = append(c.Notes, v)
			}
		}
		p.Chords = append(p.Chords, c)
	}
	return p, true
}

// Sounding is the chord actually sounding, or -1. `playing` alone is not
// enough: it keeps the LAST chord after a stop (seen on device: playing=0
// with running=0), and a held pad's audition sounds with running=0. So: the
// playing chord while the transport runs, or the chord the pad is holding.
func (p Prog) Sounding(held int) int {
	if p.Running || (held >= 0 && p.Playing == held) {
		return p.Playing
	}
	return -1
}

// Starts returns each chord's onset in units, back to back from 0.
func (p Prog) Starts() []int {
	at := make([]int, len(p.Chords))
	u := 0
	for i, c := range p.Chords {
		at[i] = u
		u += c.Len
	}
	return at
}

// MusicUnits is how much music there is; the clip may be longer (it tiles).
func (p Prog) MusicUnits() int {
	u := 0
	for _, c := range p.Chords {
		u += c.Len
	}
	return u
}

// Hits returns the onsets of a chord's rhythm, in units from the chord's
// start, as the scheduler plays them: the figure belongs to the BAR and
// tiles across the chord; "hold" (mask 1) strikes once. Port of
// web_ui.html's hitSegs, so the surfaces cannot disagree.
func Hits(c Chord, phaseU, unitsPerBar int) []float64 {
	if unitsPerBar < 1 {
		unitsPerBar = 8
	}
	if c.Mask == 1 {
		return []float64{0}
	}
	ph := phaseU % unitsPerBar
	tiles := (c.Len + ph + unitsPerBar - 1) / unitsPerBar
	var xs []float64
	for t := 0; t < tiles; t++ {
		for b := 0; b < 16; b++ {
			if c.Mask&(1<<b) == 0 {
				continue
			}
			u := float64(t*unitsPerBar) + float64(b)/16*float64(unitsPerBar) - float64(ph)
			if u < 0 {
				continue
			}
			if u >= float64(c.Len) {
				break
			}
			xs = append(xs, u)
		}
	}
	return xs
}

// InvAuto reports a chord whose inversion Stacks overrides: under a
// "dynamic" grouping (dynamic, dyn +1oct, dyn +2oct) each chord is voiced
// to move least from the one before, trying every rotation, so the stored
// inversion stops mattering -- seen on device as detents that change
// nothing. `prog` leads each chord from the one before it unless that one
// is a rest, so the first chord, and a chord after a rest, keep theirs.
// i is 0-based.
func (p Prog) InvAuto(i int, grouping string) bool {
	return strings.HasPrefix(grouping, "dyn") && i >= 1 && i < len(p.Chords) && !p.Chords[i-1].Rest()
}
