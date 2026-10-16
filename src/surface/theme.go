package surface

import (
	"image/color"

	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

// The palette is small on purpose, and every hue means one thing.
//
//	AMBER   focus: the selected chord, the active tab, the turned encoder.
//	        Nothing else is ever amber, on the screen or on the pads.
//	FUNCTION the chord's harmonic role: tonic, predominant, dominant,
//	        outside the scale. The same four hues light its pad.
//	NEUTRALS four steps of grey for everything structural.
//
// Hues are Push's own named palette entries (DESIGN.md: a colour on screen
// should be traceable to a real Push colour). Neutrals are not LED colours,
// so they are plain greys, chosen far enough apart to survive BGR565.
var (
	Ground   = color.NRGBA{0, 0, 0, 255}
	Panel    = color.NRGBA{18, 22, 26, 255}
	Rule     = color.NRGBA{44, 52, 60, 255}
	Dim      = color.NRGBA{104, 116, 128, 255}
	Ink      = color.NRGBA{200, 208, 216, 255}
	Bright   = color.NRGBA{244, 247, 250, 255}
	Focus    = named("amber")
	FocusDim = mix(named("amber"), Ground, 0.55)
)

// FuncHue is a function's colour at full strength (the sounding chord, the
// pad you are holding) and at rest.
func FuncHue(f Function) (full, rest color.NRGBA) {
	var c color.NRGBA
	switch f {
	case FuncTonic:
		c = named("sky")
	case FuncPredominant:
		c = named("green")
	case FuncDominant:
		c = named("red")
	case FuncOutside:
		c = named("violet")
	default:
		return Dim, Rule
	}
	return c, mix(c, Ground, 0.5)
}

// FuncPad is the same role as a pad LED: palette index, full and resting.
func FuncPad(f Function) (full, rest uint8) {
	pick := func(a, b string) (uint8, uint8) {
		x, _ := push3.ColorByName(a)
		y, _ := push3.ColorByName(b)
		return x, y
	}
	switch f {
	case FuncTonic:
		return pick("sky", "navy_dark")
	case FuncPredominant:
		return pick("green", "forest")
	case FuncDominant:
		return pick("red", "red_dark")
	case FuncOutside:
		return pick("violet", "magenta_dk")
	}
	return pick("gray_blue", "off")
}

func named(n string) color.NRGBA {
	idx, ok := push3.ColorByName(n)
	if !ok {
		return Ink
	}
	return push3.ColorForIndex(idx).RGB
}

// mix moves a toward b by t (0 = a, 1 = b).
func mix(a, b color.NRGBA, t float64) color.NRGBA {
	l := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5) }
	return color.NRGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 255}
}
