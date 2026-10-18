package surface

import "strings"

// Function is a chord's harmonic role in the key: what its colour says on
// the screen and on its pad. Read from the root alone, so it is a fact of
// the progression, not a judgement about voicing.
type Function int

const (
	FuncNone        Function = iota // a rest
	FuncTonic                       // home: I, iii, vi (and III, VI in minor)
	FuncPredominant                 // leading away: ii, IV
	FuncDominant                    // pulling back: V, vii, bVII
	FuncOutside                     // the root is not in the scale
)

var pcNames = [12]string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}

// rootPC is the pitch class the chord's root names ("F#4" -> 6), or -1.
func rootPC(root string) int {
	if root == "" {
		return -1
	}
	base := map[byte]int{'C': 0, 'D': 2, 'E': 4, 'F': 5, 'G': 7, 'A': 9, 'B': 11}
	pc, ok := base[root[0]]
	if !ok {
		return -1
	}
	if len(root) > 1 {
		switch root[1] {
		case '#':
			pc++
		case 'b':
			pc--
		}
	}
	return (pc + 12) % 12
}

// RootPC is the pitch class a chord's name or root names ("F#m7", "F#4"
// -> 6), or -1.
func RootPC(s string) int { return rootPC(s) }

// interval is the root's distance above the key, 0..11, or -1.
func interval(c Chord, key int) int {
	pc := rootPC(c.Root)
	if pc < 0 {
		return -1
	}
	return ((pc-key)%12 + 12) % 12
}

// FunctionOf classifies a chord. A root outside the scale is Outside
// whatever its interval: that is the information worth seeing.
func FunctionOf(c Chord, key, scaleMask int) Function {
	if c.Rest() {
		return FuncNone
	}
	iv := interval(c, key)
	if iv < 0 {
		return FuncNone
	}
	if scaleMask != 0 && scaleMask&(1<<iv) == 0 {
		return FuncOutside
	}
	switch iv {
	case 0, 3, 4, 8, 9:
		return FuncTonic
	case 2, 5:
		return FuncPredominant
	case 7, 10, 11:
		return FuncDominant
	}
	return FuncOutside
}

// Degree is a root's scale degree as a roman numeral and an accidental
// (-1 flat, +1 sharp, 0 none). A root in a seven-note scale is numbered by
// its place in THAT scale (A minor: C is III, not bIII); anything else is
// numbered against the major scale with an accidental.
func Degree(c Chord, key, scaleMask int) (roman string, accidental int) {
	romans := [7]string{"I", "II", "III", "IV", "V", "VI", "VII"}
	iv := interval(c, key)
	if iv < 0 || c.Rest() {
		return "", 0
	}
	if scaleMask != 0 && scaleMask&(1<<iv) != 0 && popcount(scaleMask) == 7 {
		d := popcount(scaleMask & (1<<iv - 1))
		return romans[d], 0
	}
	major := [12]struct {
		d, acc int
	}{{0, 0}, {1, -1}, {1, 0}, {2, -1}, {2, 0}, {3, 0}, {4, -1}, {4, 0}, {5, -1}, {5, 0}, {6, -1}, {6, 0}}
	m := major[iv]
	return romans[m.d], m.acc
}

func popcount(v int) int {
	n := 0
	for ; v != 0; v &= v - 1 {
		n++
	}
	return n
}

// DisplayName spells a chord for an UPPERCASE-ONLY face without ambiguity.
// Stacks' rule on Move, carried over: quality is spelled in full, because
// "F#M" could be either. Minor "m" -> "MIN", "maj" -> "MAJ", and an
// altered tone's flat or sharp -> "-" or "+" (jazz notation: C7-9).
// The root's own sharp stays "#".
func DisplayName(name string) string {
	if name == "" {
		return ""
	}
	root, rest := name[:1], name[1:]
	if len(rest) > 0 && (rest[0] == '#' || rest[0] == 'b') {
		root, rest = name[:2], name[2:]
	}
	root = strings.ToUpper(root[:1]) + strings.ReplaceAll(root[1:], "b", "B")
	var q strings.Builder
	for i := 0; i < len(rest); {
		// Whole words first: the "m" in "dim" is not a minor.
		if w := wordAt(rest[i:]); w != "" {
			q.WriteString(strings.ToUpper(w))
			i += len(w)
			continue
		}
		switch {
		case strings.HasPrefix(rest[i:], "maj"), strings.HasPrefix(rest[i:], "Maj"):
			q.WriteString("MAJ")
			i += 3
		case strings.HasPrefix(rest[i:], "min"):
			q.WriteString("MIN")
			i += 3
		case rest[i] == 'm':
			q.WriteString("MIN")
			i++
		case rest[i] == 'b' && i+1 < len(rest) && rest[i+1] >= '0' && rest[i+1] <= '9':
			q.WriteByte('-')
			i++
		case rest[i] == '#':
			q.WriteByte('+')
			i++
		default:
			q.WriteString(strings.ToUpper(rest[i : i+1]))
			i++
		}
	}
	return root + q.String()
}

func wordAt(s string) string {
	for _, w := range []string{"dim", "aug", "sus", "add", "alt"} {
		if strings.HasPrefix(s, w) {
			return w
		}
	}
	return ""
}

// PitchNames lists a chord's sounding pitch classes, lowest first, without
// repeats: "A C# E G#".
func PitchNames(c Chord) string {
	seen := map[int]bool{}
	var out []string
	for _, n := range c.Notes {
		pc := n % 12
		if !seen[pc] {
			seen[pc] = true
			out = append(out, pcNames[pc])
		}
	}
	return strings.Join(out, " ")
}
