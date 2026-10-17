package surface

import (
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

	numRow   = stY0 + 12 // numeral baseline, the top of every chord
	laneTop  = stY0 + 17
	laneBot  = stY1 - 24
	nameBase = stY1 - 4 // the name, at the foot of its chord
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
		drawPatternsHead(img)
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

// LaneRange is the staff's window, in lanes (degrees), and what those
// degrees were counted against. Zero means "fit to the notes".
type LaneRange struct {
	Lo, Hi    int
	Key, Mask int
	Mode      string
}

// FitLanes keeps the staff still. Fitting every frame meant changing one
// chord (its inversion, say) rescaled the lanes under all the others --
// they moved though nothing about them had. So the window only grows, to
// take in a note that leaves it, and is fitted afresh only when it would
// hold more than an octave of nothing, or the key, scale or lane mode
// change. A sweep of one chord's inversions settles after one pass.
func FitLanes(p Prog, mode string, prev LaneRange) LaneRange {
	st := lanesFor(p, mode)
	lo, hi := 1<<30, -1
	for _, c := range p.Chords {
		for _, n := range c.Notes {
			lo, hi = min(lo, st.degree[n]), max(hi, st.degree[n])
		}
	}
	if hi < 0 {
		lo, hi = st.degree[57], st.degree[69]
	}
	lo, hi = lo-1, hi+1 // a lane of air either side
	octave := st.degree[72] - st.degree[60]
	fit := LaneRange{Lo: lo, Hi: hi, Key: p.Key, Mask: p.ScaleMask, Mode: mode}
	if prev.Hi <= prev.Lo || prev.Key != p.Key || prev.Mask != p.ScaleMask || prev.Mode != mode {
		return fit
	}
	// Grow to take the new notes in, keeping what is there: a refit to the
	// notes alone would drop the room a sweep had already made, and the
	// sweep back would jump the lanes again.
	grown := prev
	grown.Lo, grown.Hi = min(prev.Lo, lo), max(prev.Hi, hi)
	if (grown.Hi-grown.Lo)-(hi-lo) > octave {
		return fit // more than an octave of nothing: tighten
	}
	return grown
}

func newStaff(p Prog, lanes string, win LaneRange) staff {
	st := lanesFor(p, lanes)
	if win.Hi <= win.Lo || win.Key != p.Key || win.Mask != p.ScaleMask || win.Mode != lanes {
		win = FitLanes(p, lanes, LaneRange{})
	}
	lo, hi := win.Lo, win.Hi
	h := laneBot - laneTop
	st.step = min(max(h/max(hi-lo, 1), 2), 7)
	st.lo, st.hi = lo, hi
	st.top = laneTop + (h-st.step*(hi-lo))/2
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
			gfx.FillRect(img, 0, y, W, 1, Rule)
			continue
		}
		// The key's root is the reference line, brighter, with a
		// registration mark at each end of the screen.
		gfx.FillRect(img, 0, y, W, 1, mix(Dim, Rule, 0.3))
		gfx.FillRect(img, 0, y-1, 3, 3, Dim)
		gfx.FillRect(img, W-3, y-1, 3, 3, Dim)
	}
	for _, b := range blocks {
		drawChord(img, b.x0, b.x1, b.i, starts[b.i], p, st, b.ghost)
	}
	// The clip loops the phrase: say how often, at the top right.
	if clip > music {
		loops := "X" + itoa((clip+music-1)/music)
		if clip%music != 0 {
			loops = itoa(clip/p.StepUnits) + " BAR CLIP"
		}
		right(img, W-6, numRow, loops, Dim)
	}
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

func drawChord(img *image.NRGBA, x0, x1, i, start int, p Prog, st staff, ghost bool) {
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
		for _, n := range c.Notes {
			y := st.laneY(st.degree[n])
			if c.Mute { // an outline: you still see what it would play
				gfx.FillRect(img, hx0, y-1, hx1-hx0, 1, Dim)
				gfx.FillRect(img, hx0, y+1, hx1-hx0, 1, Dim)
				continue
			}
			gfx.FillRect(img, hx0, y-1, hx1-hx0, 3, note)
			if !st.inScale[n] && k == 0 && !ghost { // the accidental mark
				gfx.FillRect(img, hx0-3, y-1, 2, 3, Bright)
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
	if roman, acc := Degree(c, p.Key, p.ScaleMask); roman != "" && w >= 24 {
		dx := x0 + 6
		if acc != 0 {
			drawAccidental(img, dx, numRow-1, acc, full)
			dx += 6
		}
		text.Draw(img, dx, numRow, roman, full)
	}
	// The name at its foot, as large as the chord is long.
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
	switch {
	case text.WidthScaled(name, 2) <= w-12:
		text.DrawScaled(img, x0+6, nameBase, 2, name, col)
	case text.Width(name) <= w-10:
		text.Draw(img, x0+6, nameBase-4, name, col)
	case w >= 20:
		text.Draw(img, x0+6, nameBase-4, text.Truncate(name, (w-10)/7), col)
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
		if a.Lit != nil && a.Lit(s.Params, p) {
			gfx.FillRect(img, x+2, actY+2, ColW-4, actH-3, Ink)
			col = Ground
		}
		center(img, x, ColW, actY+13, a.Label, col)
	}
}

// ---- patterns ----------------------------------------------------------

func drawPatternsHead(img *image.NRGBA) {
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
	h := (stY1 - stY0) / 2
	for i := 0; i < 16; i++ {
		x, y := (i%8)*ColW, stY0+(1-i/8)*h // bottom row is 1-8, as on the pads
		used := mask&(1<<i) != 0
		gfx.FillRect(img, x+2, y+1, ColW-4, h-2, Panel)
		label, col := "EMPTY", Rule
		if used {
			label, col = "MUSIC", Ink
		}
		if i+1 == cur {
			frame(img, x+2, y+1, ColW-4, h-2, Focus)
			label, col = "ACTIVE", Focus
		}
		numCol := Dim
		if used || i+1 == cur {
			numCol = Bright
		}
		text.DrawScaled(img, x+9, y+33, 2, pad2(i+1), numCol)
		right(img, x+ColW-9, y+15, label, col)
	}
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
