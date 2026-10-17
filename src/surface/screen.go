package surface

import (
	"hash/fnv"
	"image"
	"image/color"
	"strings"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
)

// The screen has two kinds of space, and they do not mix.
//
// CONTROL space is Push's grid: eight 120 px columns, each true to a
// physical control -- a tab under each top button, a readout under each
// encoder, an action over each bottom button. A column means "this
// control", so nothing else is ever laid out on it.
//
// MUSIC space is the staff between them: true time across the full width,
// every chord exactly as long as it lasts, its notes on the scale's lanes.
// The progression owes nothing to the column grid; aligning chords to it
// would say encoder 3 has something to do with chord 3.
//
//	tabs       0-13   one per top button: the bank
//	encoders  14-49   one per encoder: full name, value, gauge
//	staff     51-141  the music, true to time
//	actions  142-159  one per bottom button
const (
	W, H = 960, 160
	ColW = W / 8

	tabY, tabH = 0, 14
	encY, encH = 14, 36
	stY0, stY1 = 51, 142
	actY, actH = 142, 18

	numRow = stY0 + 12 // numeral and name baseline, the top of every chord
)

// The lanes' band. The chord's name sat at its foot at 2x and took 24 px
// of it; on top, beside its numeral, the lanes get 71 px instead of 50 --
// the difference between 2 px lanes and 4 px ones for a progression that
// spans two and a half octaves (compared side by side, 2026-10-17).
const (
	laneTop = stY0 + 16
	laneBot = stY1 - 4
)

// Scene is everything one frame needs. Params is the module's flat state
// map, exactly as the Tool tab gets it.
type Scene struct {
	M       *Manifest
	Params  map[string]string
	Tab     int       // index into M.Banks, or TabPatterns
	Touched int       // encoder being touched, -1 none
	Head    float64   // playhead in units, interpolated between reads
	Toast   string    // a transient answer to something just done
	Blink   bool      // the slow blink phase, for ARMED
	Held    int       // chord index a pad is holding (auditioning), -1 none
	Scroll  int       // start of the page's long editor row, -1 keeps the value in view
	Legend  bool      // a scene button is held: draw the pad map of Page instead
	Page    EditPage  // the editor page the pads show
	Lanes   LaneRange // the staff's window, from FitLanes; zero fits it afresh
	PatMode string    // PATTERNS: "clear" or "copy" armed for the next slot tapped, "" none
}

const (
	TabStart    = 1
	TabFeel     = 4
	TabClip     = 5
	TabPatterns = 6
)

// unavailable lists controls whose Move mechanism Push lacks: Read Clip
// parses Move's Song.abl. They will come back through Browser Bridge.
var unavailable = map[string]bool{"read": true, "read_mode": true, "read_track": true}

// tabs is what the 8 top buttons say: the six banks, PATTERNS, and LIVE
// (park Stacks and give the surface back to Live).
func (s Scene) tabs() []string {
	var t []string
	for _, b := range s.M.Banks {
		t = append(t, b.Name)
	}
	return append(t, "PATTERNS", "LIVE")
}

// Render draws one frame.
func Render(img *image.NRGBA, s Scene) {
	if s.Legend {
		drawLegend(img, s)
		return
	}
	gfx.FillRect(img, 0, 0, W, H, Ground)
	p, ok := ParseProg(s.Params["prog"])
	p.Playing = p.Sounding(s.Held)
	drawTabs(img, s)
	if s.Tab == TabPatterns {
		drawPatternsHead(img, s.PatMode)
	} else {
		drawEncoders(img, s)
	}
	gfx.FillRect(img, 0, stY0-1, W, 1, Rule)
	switch {
	case !ok:
		center(img, 0, W, (stY0+stY1)/2+4, "NO PROGRESSION", Dim)
	case s.Tab == TabPatterns:
		drawPatterns(img, s)
	case s.Tab == TabStart:
		drawLibrary(img, s, p)
	default:
		drawStaff(img, s, p)
	}
	if s.Toast != "" {
		drawToast(img, s.Toast, s.Blink)
	} else {
		drawActions(img, s, p)
	}
}

// ---- tabs --------------------------------------------------------------

func drawTabs(img *image.NRGBA, s Scene) {
	for i, name := range s.tabs() {
		x := i * ColW
		col := Dim
		if i == s.Tab {
			col = Bright
			gfx.FillRect(img, x+8, tabY+tabH-2, ColW-16, 2, Focus)
		}
		center(img, x, ColW, tabY+10, name, col)
		if i > 0 {
			gfx.FillRect(img, x, tabY+3, 1, tabH-6, Rule) // registration tick
		}
	}
}

// ---- encoders ----------------------------------------------------------

func drawEncoders(img *image.NRGBA, s Scene) {
	b := s.M.Banks[s.Tab]
	for i, key := range b.Knobs {
		x := i * ColW
		if i > 0 {
			gfx.FillRect(img, x, encY+4, 1, encH-8, Rule)
		}
		if key == "" {
			continue
		}
		p := s.M.Params[key]
		val := s.Params[key]
		touched := i == s.Touched
		labelCol := Dim
		if touched {
			labelCol = Focus
		}
		text.Draw(img, x+8, encY+12, text.Truncate(p.Label(), 15), labelCol)

		if unavailable[key] {
			text.Draw(img, x+8, encY+28, "NOT ON PUSH YET", Dim)
			continue
		}
		if p.Trigger {
			text.Draw(img, x+8, encY+28, "TURN TO FIRE", Ink)
			continue
		}
		v := strings.ToUpper(val)
		if p.Bipolar() && !strings.HasPrefix(val, "-") && val != "0" && val != "" {
			v = "+" + v
		}
		if key == "inv" && invAuto(s) {
			// Voice leading picks the inversion: say so, and keep the stored
			// value small beside it (it returns if grouping leaves dynamic).
			text.DrawScaled(img, x+8, encY+31, 2, "AUTO", Dim)
			right(img, x+ColW-8, encY+31, v, Dim)
			continue
		}
		if text.WidthScaled(v, 2) <= ColW-16 {
			text.DrawScaled(img, x+8, encY+31, 2, v, Bright)
		} else {
			text.Draw(img, x+8, encY+28, text.Truncate(v, 15), Bright)
		}
		drawGauge(img, x+8, encY+encH-3, ColW-16, p, val, touched)
	}
}

// drawGauge is a 2 px track: a fill for a range, a centre-out fill for a
// bipolar one, and a single mark for a list of named options -- a list has
// a place, not an amount.
func drawGauge(img *image.NRGBA, x, y, w int, p Param, val string, touched bool) {
	f, ok := p.Frac(val)
	if !ok {
		return
	}
	gfx.FillRect(img, x, y, w, 2, Rule)
	col := Ink
	if touched {
		col = Focus
	}
	switch {
	case p.Type == "enum" && len(p.Options) > 2:
		n := len(p.Options)
		if n <= 24 { // ticks for every option while they stay legible
			for i := 0; i < n; i++ {
				tx := x + int(float64(i)/float64(n-1)*float64(w-1))
				gfx.FillRect(img, tx, y+2, 1, 1, Rule)
			}
		}
		gfx.FillRect(img, x+int(f*float64(w-4)), y-1, 4, 4, col)
	case p.Bipolar():
		mid, pos := x+w/2, x+int(f*float64(w))
		lo, hi := min(mid, pos), max(mid, pos)
		gfx.FillRect(img, mid, y-2, 1, 6, Dim)
		gfx.FillRect(img, lo, y, hi-lo, 2, col)
	default:
		gfx.FillRect(img, x, y, int(f*float64(w)), 2, col)
	}
}

// ---- the staff ---------------------------------------------------------

// staff maps a note to its lane: one lane per scale degree (Note Lanes =
// scale) or per letter name (diatonic). Ported from canvas.js
// buildDegrees: a note outside the scale shares the degree below it and is
// marked, which is what an accidental is for.
type staff struct {
	inScale [128]bool
	degree  [128]int
	tonic   map[int]bool
	lo, hi  int
	step    int
	top     int
}

// LaneRange is the staff's window: the lowest lane shown, the pixels per
// lane, and what the lanes were counted against. Zero means "fit afresh".
type LaneRange struct {
	Lo, Hi    int // lanes (degrees) at the foot and the head of the window
	Step      int // pixels per lane, one of laneSteps
	Key, Mask int
	Mode      string
	Harmony   uint64 // the chords' names, hashed: what may re-frame the zoom
}

// laneSteps are the only spacings the staff uses, largest first. A
// spacing worked out from the notes (height / span) changed with every
// edit: one chord inverted, and every chord's lanes drew closer or
// further apart and slid -- reported on device as "totally inconsistent".
// With a few fixed steps an interval is the same height from one edit to
// the next, and only a real change of room changes it.
var laneSteps = []int{6, 5, 4, 3, 2}

// lanesAt is how many lane intervals a step shows.
func lanesAt(step int) int { return (laneBot - laneTop) / step }

// fitStep is the largest step that shows span lanes; the smallest if none.
func fitStep(span int) int {
	for _, s := range laneSteps {
		if span <= lanesAt(s) {
			return s
		}
	}
	return laneSteps[len(laneSteps)-1]
}

// FitLanes moves the window as little as the notes allow. In order:
//
//   - key, scale or lane mode changed (or nothing before): fit afresh,
//     the largest step that shows every note with headroom, centred;
//   - the notes outgrew the window: zoom out, about the window's centre,
//     so what did not change stays near where it was;
//   - the HARMONY changed (a chord's name, the number of chords -- a new
//     degree, a progression or pattern loaded) and the notes fit a larger
//     step: zoom in, about the centre;
//   - otherwise only pan, and by whole octaves when that is enough, so the
//     tonic lines keep their rows and nothing shifts.
//
// So voicing -- inversion, octave, colour, grouping -- never zooms in. A
// first version zoomed in whenever the notes fit a larger step with an
// octave to spare; a sweep over several chords' inversions swings the span
// by more than an octave, and the zoom went out and back in every pass
// (TestLanesHoldStillThroughASweep). No margin is safe against a wider
// sweep; tying zoom-in to the harmony is.
func FitLanes(p Prog, mode string, prev LaneRange) LaneRange {
	st := lanesFor(p, mode)
	lo, hi := noteSpan(p, st)
	span := hi - lo
	octave := st.degree[72] - st.degree[60]
	harmony := harmonyOf(p)
	at := func(step, wlo int) LaneRange {
		return LaneRange{Lo: wlo, Hi: wlo + lanesAt(step), Step: step, Key: p.Key, Mask: p.ScaleMask,
			Mode: mode, Harmony: harmony}
	}
	// Whenever a zoom is chosen it leaves half an octave of headroom: a fit
	// to the notes alone made the first inversion edit zoom out, and the
	// next, and the next (6, 5, 4, 3 px over one sweep on device data).
	want := fitStep(span + (octave+1)/2)
	if prev.Step == 0 || prev.Key != p.Key || prev.Mask != p.ScaleMask || prev.Mode != mode {
		return panTo(at(want, lo-(lanesAt(want)-span)/2), lo, hi, octave)
	}
	step := prev.Step
	if span > lanesAt(step) || (harmony != prev.Harmony && want > step) {
		step = want
	}
	win := prev
	win.Harmony = harmony
	if step != prev.Step {
		twiceMid := 2*prev.Lo + lanesAt(prev.Step)
		win = at(step, (twiceMid-lanesAt(step))/2)
	}
	return panTo(win, lo, hi, octave)
}

// harmonyOf hashes the chords' names: what a voicing edit leaves alone.
func harmonyOf(p Prog) uint64 {
	h := fnv.New64a()
	for _, c := range p.Chords {
		h.Write([]byte(c.Name))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// panTo shifts the window the least that shows lo..hi, by whole octaves
// when that shows them too.
func panTo(w LaneRange, lo, hi, octave int) LaneRange {
	if lo >= w.Lo && hi <= w.Hi {
		return w
	}
	n := w.Hi - w.Lo
	var d int
	switch {
	case hi-lo > n: // cannot show them all (past the smallest step): centre
		d = lo - (n-(hi-lo))/2 - w.Lo
	case lo < w.Lo:
		d = lo - w.Lo
	default:
		d = hi - w.Hi
	}
	if octave > 0 && hi-lo <= n {
		k := (abs(d) + octave - 1) / octave * octave
		if d < 0 {
			k = -k
		}
		if lo >= w.Lo+k && hi <= w.Hi+k {
			d = k
		}
	}
	w.Lo, w.Hi = w.Lo+d, w.Hi+d
	return w
}

// noteSpan is the lanes the notes need, with a lane of air either side.
func noteSpan(p Prog, st staff) (lo, hi int) {
	lo, hi = 1<<30, -1
	for _, c := range p.Chords {
		for _, n := range c.Notes {
			lo, hi = min(lo, st.degree[n]), max(hi, st.degree[n])
		}
	}
	if hi < 0 {
		lo, hi = st.degree[57], st.degree[69]
	}
	return lo - 1, hi + 1
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func newStaff(p Prog, lanes string, win LaneRange) staff {
	st := lanesFor(p, lanes)
	if win.Step == 0 || win.Key != p.Key || win.Mask != p.ScaleMask || win.Mode != lanes {
		win = FitLanes(p, lanes, LaneRange{})
	}
	st.step, st.lo, st.hi = win.Step, win.Lo, win.Hi
	st.top = laneTop + (laneBot-laneTop-st.step*(st.hi-st.lo))/2
	return st
}

// lanesFor numbers the lanes: every MIDI note's degree, and which are tonic.
func lanesFor(p Prog, lanes string) staff {
	mask, key := p.ScaleMask, p.Key
	if lanes == "diatonic" || mask == 0 {
		mask, key = 0xAB5, 0 // the seven letter names
	}
	st := staff{tonic: map[int]bool{}}
	d := -1
	for n := 0; n < 128; n++ {
		pc := ((n-key)%12 + 12) % 12
		on := mask&(1<<pc) != 0
		if on {
			d++
		}
		st.inScale[n] = on
		st.degree[n] = max(d, 0)
		if on && ((n-p.Key)%12+12)%12 == 0 {
			st.tonic[st.degree[n]] = true
		}
	}
	return st
}

func (st staff) laneY(d int) int { return st.top + (st.hi-d)*st.step }

// minChordW keeps the shortest chord legible: room for its numeral and a
// 1x name. Below it, a chord borrows width from the long ones.
const minChordW = 56

// Spans lays the phrase across the full width: every chord in proportion
// to its length, so four chords take a quarter each and sixteen a
// sixteenth, and a two-bar chord stays twice a one-bar one. A chord that
// would fall under minChordW is held there and the rest share what is
// left. Returns len(chords)+1 edges, from 0 to W.
func Spans(p Prog) []int {
	n := len(p.Chords)
	edges := make([]int, n+1)
	if n == 0 {
		return edges
	}
	fixed := make([]bool, n)
	widths := make([]float64, n)
	for pass := 0; pass < n; pass++ { // settle the ones held at the floor
		free, units := float64(W), 0
		for i, c := range p.Chords {
			if fixed[i] {
				free -= minChordW
			} else {
				units += c.Len
			}
		}
		changed := false
		for i, c := range p.Chords {
			if fixed[i] {
				widths[i] = minChordW
				continue
			}
			widths[i] = free * float64(c.Len) / float64(max(units, 1))
			if widths[i] < minChordW && float64(n*minChordW) <= W {
				fixed[i], changed = true, true
			}
		}
		if !changed {
			break
		}
	}
	at := 0.0
	for i := range widths {
		at += widths[i]
		edges[i+1] = int(at + 0.5)
	}
	edges[n] = W
	return edges
}

// drawStaff is the music: the phrase across the full width, laid out by
// Spans. Where the clip loops the phrase, a marker says how many times
// rather than spending width on repeats.
func drawStaff(img *image.NRGBA, s Scene, p Prog) {
	clip, music := p.ClipUnits, p.MusicUnits()
	if clip < 1 || music < 1 {
		return
	}
	st := newStaff(p, s.Params["lanes"], s.Lanes)
	starts := p.Starts()
	edges := Spans(p)

	type block struct {
		i, x0, x1 int
		ghost     bool
	}
	var blocks []block
	for i := range p.Chords {
		blocks = append(blocks, block{i, edges[i], edges[i+1], false})
	}

	// Ground, then lanes across the full width so the staff is continuous,
	// then the chords on it.
	for _, b := range blocks {
		bg := Panel
		if b.i == p.Playing && !b.ghost {
			_, rest := FuncHue(FunctionOf(p.Chords[b.i], p.Key, p.ScaleMask))
			bg = mix(rest, Ground, 0.6)
		}
		if b.ghost {
			bg = mix(Panel, Ground, 0.5)
		}
		gfx.FillRect(img, b.x0+1, stY0, b.x1-b.x0-1, stY1-stY0-1, bg)
	}
	for d := st.lo; d <= st.hi; d++ {
		y := st.laneY(d)
		if !st.tonic[d] {
			lane := Rule
			if st.step <= 3 { // close lanes: a fainter rule, or they read as a fill
				lane = mix(Rule, Ground, 0.45)
			}
			gfx.FillRect(img, 0, y, W, 1, lane)
			continue
		}
		// The key's root is the reference line, brighter, with a
		// registration mark at the left edge (the right carries the register bar).
		gfx.FillRect(img, 0, y, W, 1, mix(Dim, Rule, 0.3))
		gfx.FillRect(img, 0, y-1, 3, 3, Dim)
	}
	// The clip loops the phrase: say how often, at the top right -- the
	// last chord's name gives way to it.
	loops := ""
	if clip > music {
		loops = "X" + itoa((clip+music-1)/music)
		if clip%music != 0 {
			loops = itoa(clip/p.StepUnits) + " BAR CLIP"
		}
		right(img, W-6, numRow, loops, Dim)
	}
	for _, b := range blocks {
		reserve := 0
		if b.i == len(p.Chords)-1 && loops != "" {
			reserve = text.Width(loops) + 8
		}
		drawChord(img, b.x0, b.x1, b.i, starts[b.i], p, st, b.ghost, reserve)
	}
	drawRegister(img, st)
	// The playhead, at its place in the phrase.
	if p.Running || s.Held >= 0 || s.Head > 0 {
		u := s.Head
		for u >= float64(music) {
			u -= float64(music)
		}
		for i, c := range p.Chords {
			if u < float64(starts[i]+c.Len) {
				f := (u - float64(starts[i])) / float64(c.Len)
				hx := edges[i] + int(f*float64(edges[i+1]-edges[i]))
				gfx.FillRect(img, hx, stY0, 2, stY1-stY0-1, Bright)
				break
			}
		}
	}
}

func drawChord(img *image.NRGBA, x0, x1, i, start int, p Prog, st staff, ghost bool, reserve int) {
	c := p.Chords[i]
	f := FunctionOf(c, p.Key, p.ScaleMask)
	full, _ := FuncHue(f)
	selected := i+1 == p.Sel && !ghost
	playing := i == p.Playing && !ghost
	w := x1 - x0

	note := mix(full, Ground, 0.25) // a chord at rest still shows its role
	switch {
	case playing:
		note = mix(full, Bright, 0.2)
	case ghost:
		note = mix(full, Ground, 0.7)
	}

	// The notes on their lanes, drawn as the hits they play: the figure
	// belongs to the bar and tiles across the chord.
	nx, nw := x0+6, w-11
	hits := Hits(c, start, p.StepUnits)
	for k, u := range hits {
		hx0 := nx + int(u/float64(c.Len)*float64(nw))
		hx1 := nx + nw
		if k+1 < len(hits) {
			hx1 = nx + int(hits[k+1]/float64(c.Len)*float64(nw)) - 2
		}
		hx1 = max(hx1, hx0+2)
		// A bar is as thick as its lane allows, so neighbours never merge.
		t := min(3, max(1, st.step-1))
		for _, n := range c.Notes {
			d := st.degree[n]
			if d < st.lo || d > st.hi { // beyond the smallest step's reach: a mark at the edge
				gfx.FillRect(img, hx0, st.laneY(min(max(d, st.lo), st.hi)), hx1-hx0, 1, Dim)
				continue
			}
			y := st.laneY(d) - t/2
			if c.Mute { // an outline: you still see what it would play
				gfx.FillRect(img, hx0, y, hx1-hx0, 1, Dim)
				if t > 1 {
					gfx.FillRect(img, hx0, y+t-1, hx1-hx0, 1, Dim)
				}
				continue
			}
			gfx.FillRect(img, hx0, y, hx1-hx0, t, note)
			if !st.inScale[n] && k == 0 && !ghost { // the accidental mark
				gfx.FillRect(img, hx0-3, y, 2, t, Bright)
			}
		}
	}
	if ghost {
		return
	}
	for b := p.StepUnits; b < c.Len; b += p.StepUnits { // bar lines inside a long chord
		bx := nx + int(float64(b)/float64(c.Len)*float64(nw))
		gfx.FillRect(img, bx, laneTop, 1, laneBot-laneTop, Rule)
		gfx.FillRect(img, bx, stY0, 1, 3, Dim)
	}
	if !c.Rest() && !c.Mute {
		gfx.FillRect(img, x0+1, stY0, w-1, 2, map[bool]color.NRGBA{true: full, false: mix(full, Ground, 0.5)}[playing])
	}

	// The degree in its function colour, where the chord begins.
	dx := x0 + 6
	if roman, acc := Degree(c, p.Key, p.ScaleMask); roman != "" && w >= 24 {
		if acc != 0 {
			drawAccidental(img, dx, numRow-1, acc, full)
			dx += 6
		}
		text.Draw(img, dx, numRow, roman, full)
		dx += text.Width(roman) + 7
	}
	name := DisplayName(c.Name)
	col := Ink
	if selected || playing {
		col = Bright
	}
	if c.Rest() {
		name, col = "REST", Dim
	}
	if c.Mute {
		col = Dim
	}
	if room := (x1 - 5 - reserve - dx) / 7; room >= 4 { // on top, after its numeral
		text.Draw(img, dx, numRow, text.Truncate(name, room), col)
	}
	if selected {
		frame(img, x0+1, stY0, w-1, stY1-stY0-1, Focus)
	}
}

// ---- actions: the bottom buttons ----------------------------------------

// Action is one bottom button: what it fires (a Stacks param set to a
// value) and whether it shows as lit.
type Action struct {
	Label      string
	Key, Value string
	Lit        func(map[string]string, Prog) bool
	Mode       string // a host-side mode it arms instead of a param ("clear", "copy")
}

// IsLit reports whether a button shows its action as on.
func (a Action) IsLit(s Scene, p Prog) bool {
	if a.Mode != "" {
		return a.Mode == s.PatMode
	}
	return a.Lit != nil && a.Lit(s.Params, p)
}

var editActions = []Action{
	{Label: "INSERT", Key: "insert", Value: "on"},
	{Label: "DELETE", Key: "remove", Value: "on"},
	{Label: "DUPLICATE", Key: "duplicate", Value: "on"},
	{Label: "HALFTIME", Key: "stretch", Value: "on"},
	{Label: "DOUBLE", Key: "compress", Value: "on"},
	{Label: "MUTE", Key: "cmute", Value: "toggle", Lit: func(m map[string]string, _ Prog) bool { return m["cmute"] == "on" }},
	{Label: "UNDO", Key: "undo", Value: "on"},
	{Label: "REDO", Key: "redo", Value: "on"},
}

// Actions is what the bottom row does on a tab. Most banks share the edit
// row -- the hand learns one row -- and FEEL and CLIP put their own
// triggers there, where a turn-to-fire knob used to be the only way.
func Actions(tab int) []Action {
	prev := func(v string) func(map[string]string, Prog) bool {
		return func(m map[string]string, _ Prog) bool { return m["preview"] == v }
	}
	switch tab {
	case TabPatterns:
		// The slots' own row. The edit row's DELETE here deleted the
		// selected CHORD -- pressed on device to clear a slot, it took the
		// pattern's chords one by one and left the slot as it was.
		return []Action{
			{Label: "CLEAR SLOT", Mode: "clear"},
			{Label: "COPY TO SLOT", Mode: "copy"},
			{}, {}, {}, {},
			editActions[6], editActions[7],
		}
	case TabFeel:
		return []Action{
			{Label: "PREV OFF", Key: "preview", Value: "off", Lit: prev("off")},
			{Label: "PREV CHORD", Key: "preview", Value: "chord", Lit: prev("chord")},
			{Label: "PREV LOOP", Key: "preview", Value: "loop", Lit: prev("loop")},
			{},
			{Label: "RANDOM VEL", Key: "roll_vel", Value: "on"},
			{Label: "RANDOM TIME", Key: "roll_time", Value: "on"},
			editActions[6], editActions[7],
		}
	case TabClip:
		return []Action{
			{Label: "STAMP", Key: "stamp", Value: "on", Lit: func(m map[string]string, _ Prog) bool { return m["status"] == "armed" }},
			{Label: "CLEAR", Key: "clear", Value: "on"},
			{Label: "RUN", Key: "run", Value: "toggle", Lit: func(m map[string]string, _ Prog) bool { return m["run"] == "on" }},
			{}, {}, {},
			editActions[6], editActions[7],
		}
	}
	return editActions
}

func drawActions(img *image.NRGBA, s Scene, p Prog) {
	gfx.FillRect(img, 0, actY, W, 1, Rule)
	for i, a := range Actions(s.Tab) {
		x := i * ColW
		if i > 0 {
			gfx.FillRect(img, x, actY+4, 1, actH-8, Rule)
		}
		if a.Label == "" {
			continue
		}
		col := Ink
		if a.IsLit(s, p) {
			gfx.FillRect(img, x+2, actY+2, ColW-4, actH-3, Ink)
			col = Ground
		}
		center(img, x, ColW, actY+13, a.Label, col)
	}
}

// ---- patterns ----------------------------------------------------------

func drawPatternsHead(img *image.NRGBA, mode string) {
	if ask, ok := map[string]string{"clear": "TAP THE SLOT TO CLEAR", "copy": "TAP THE SLOT TO COPY THIS PATTERN INTO"}[mode]; ok {
		text.DrawScaled(img, 8, encY+31, 2, ask, Focus)
		right(img, W-8, encY+12, "PRESS "+map[string]string{"clear": "CLEAR SLOT", "copy": "COPY TO SLOT"}[mode]+" AGAIN TO CANCEL", Dim)
		return
	}
	cells := [][2]string{{"PATTERNS", ""}, {"", ""}, {"TAP", "GO TO SLOT"}, {"", ""},
		{"HOLD", "COPY INTO SLOT"}, {"", ""}, {"DELETE + TAP", "CLEAR SLOT"}, {"", ""}}
	text.DrawScaled(img, 8, encY+31, 2, "16 SLOTS", Bright)
	for i, c := range cells {
		if c[0] != "" {
			text.Draw(img, i*ColW+8, encY+12, c[0], Dim)
		}
		if c[1] != "" {
			text.Draw(img, i*ColW+8, encY+28, c[1], Ink)
		}
	}
}

// drawPatterns shows the sixteen buffers as the two pad rows they are --
// here the column grid is honest: slot 3 IS the third pad of its row.
func drawPatterns(img *image.NRGBA, s Scene) {
	mask, cur := parsePatterns(s.Params["patterns"])
	slots := ParseSlots(s.Params["pat_names"])
	h := (stY1 - stY0) / 2
	for i := 0; i < 16; i++ {
		x, y := (i%8)*ColW, stY0+(1-i/8)*h // bottom row is 1-8, as on the pads
		used := mask&(1<<i) != 0
		active := i+1 == cur
		gfx.FillRect(img, x+2, y+1, ColW-4, h-2, Panel)
		if active {
			frame(img, x+2, y+1, ColW-4, h-2, Focus)
		}
		numCol := Dim
		if used || active {
			numCol = Bright
		}
		text.DrawScaled(img, x+9, y+29, 2, pad2(i+1), numCol)
		sl := slots[i]
		switch {
		case !used:
			right(img, x+ColW-9, y+15, "EMPTY", Rule)
		case sl.Count == 0: // used, but the module did not say with what
		default:
			// What is in it: how much, then the chords themselves.
			right(img, x+ColW-9, y+15, itoa(sl.Count)+" CH "+itoa(sl.Bars)+" BAR", Dim)
			shown := make([]string, len(sl.Names))
			for k, n := range sl.Names {
				shown[k] = DisplayName(n)
			}
			names := strings.Join(shown, " ")
			if sl.Count > len(sl.Names) {
				names += " +" + itoa(sl.Count-len(sl.Names))
			}
			col := Ink
			if active {
				col = Focus
			}
			text.Draw(img, x+9, y+h-4, text.Truncate(names, (ColW-18)/7), col)
		}
		if active {
			right(img, x+ColW-9, y+29, "ACTIVE", Focus)
		}
	}
}

// Slot is what a pattern slot holds, from the module's `pat_names`.
type Slot struct {
	Count, Bars int
	Names       []string // the first four chords; "-" a rest
}

// ParseSlots reads `pat_names`: sixteen '|'-split fields, COUNT:BARS:NAMES
// or empty. A module without the key gives sixteen empty slots.
func ParseSlots(v string) [16]Slot {
	var out [16]Slot
	for i, f := range strings.SplitN(v, "|", 16) {
		parts := strings.SplitN(f, ":", 3)
		if len(parts) < 3 {
			continue
		}
		out[i] = Slot{Count: atoi(parts[0]), Bars: atoi(parts[1]), Names: strings.Fields(parts[2])}
	}
	return out
}

func parsePatterns(v string) (mask, cur int) {
	m, c, _ := strings.Cut(v, "|")
	for _, ch := range m {
		mask <<= 4
		switch {
		case ch >= '0' && ch <= '9':
			mask |= int(ch - '0')
		case ch >= 'a' && ch <= 'f':
			mask |= int(ch-'a') + 10
		}
	}
	return mask, atoi(c)
}

// ---- toast -------------------------------------------------------------

// drawToast is a status line in the action row's place: an answer to
// something just done, full width, never covering the music.
func drawToast(img *image.NRGBA, msg string, blink bool) {
	bg := Focus
	if blink {
		bg = FocusDim
	}
	gfx.FillRect(img, 0, actY, W, actH, bg)
	center(img, 0, W, actY+13, msg, Ground)
}

// ---- small helpers -----------------------------------------------------

// lengthLabel says a length the way a musician counts it.
func lengthLabel(units, perBar int) string {
	if perBar < 1 {
		perBar = 8
	}
	switch {
	case units%perBar == 0 && units == perBar:
		return "1 BAR"
	case units%perBar == 0:
		return itoa(units/perBar) + " BARS"
	case units*4 == perBar:
		return "1/4 BAR"
	case units*2 == perBar:
		return "1/2 BAR"
	case units*4 == perBar*3:
		return "3/4 BAR"
	}
	return itoa(units) + "/" + itoa(perBar) + " BAR"
}

func center(img *image.NRGBA, x, w, baseline int, s string, col color.NRGBA) {
	text.Draw(img, x+(w-text.Width(s))/2, baseline, s, col)
}

func right(img *image.NRGBA, xRight, baseline int, s string, col color.NRGBA) {
	text.Draw(img, xRight-text.Width(s), baseline, s, col)
}

func frame(img *image.NRGBA, x, y, w, h int, col color.NRGBA) {
	gfx.FillRect(img, x, y, w, 1, col)
	gfx.FillRect(img, x, y+h-1, w, 1, col)
	gfx.FillRect(img, x, y, 1, h, col)
	gfx.FillRect(img, x+w-1, y, 1, h, col)
}

// drawAccidental draws a flat or sharp as pixels: the face is ASCII only,
// and "B" for flat would read as the note B.
func drawAccidental(img *image.NRGBA, x, baseline, acc int, col color.NRGBA) {
	if acc < 0 { // flat: a stem and a bowl
		gfx.FillRect(img, x, baseline-9, 1, 9, col)
		gfx.FillRect(img, x+1, baseline-4, 2, 1, col)
		gfx.FillRect(img, x+3, baseline-3, 1, 2, col)
		gfx.FillRect(img, x+1, baseline-1, 2, 1, col)
		return
	}
	gfx.FillRect(img, x+1, baseline-9, 1, 9, col) // sharp
	gfx.FillRect(img, x+3, baseline-9, 1, 9, col)
	gfx.FillRect(img, x, baseline-6, 5, 1, col)
	gfx.FillRect(img, x, baseline-3, 5, 1, col)
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

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

// invAuto: the selected chord's inversion is picked by voice leading.
func invAuto(s Scene) bool {
	p, ok := ParseProg(s.Params["prog"])
	return ok && p.InvAuto(p.Sel-1, s.Params["grouping"])
}

// drawRegister is the window's place in the whole register, as a scroll
// bar at the right edge: a track for the piano's range (A0-C8) and a mark
// for the lanes on screen. The window pans by whole octaves to keep the
// lanes still, so moving everything an octave (Octave Default) can leave
// the staff looking the same -- the bar is what shows it moved.
func drawRegister(img *image.NRGBA, st staff) {
	const x, w = W - 3, 3
	lo, hi := st.degree[21], st.degree[108]
	y0, y1 := laneTop, laneBot
	gfx.FillRect(img, x, y0, w, y1-y0, Panel)
	gfx.FillRect(img, x+1, y0, 1, y1-y0, Rule)
	at := func(d int) int { return y1 - (y1-y0)*(min(max(d, lo), hi)-lo)/max(hi-lo, 1) }
	top, bot := at(st.hi), at(st.lo)
	gfx.FillRect(img, x, top, w, max(bot-top, 2), Dim)
}
