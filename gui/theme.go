package gui

import (
	"image/color"

	"gioui.org/font"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// Spacing scale. Every gap in the UI is one of these; ad-hoc values are what
// makes an interface feel noisy.
const (
	SpaceXS  unit.Dp = 4
	SpaceSM  unit.Dp = 8
	SpaceMD  unit.Dp = 12
	SpaceLG  unit.Dp = 16
	SpaceXL  unit.Dp = 24
	Space2XL unit.Dp = 32
)

// Corner radii.
const (
	RadiusSM   unit.Dp = 6
	RadiusMD   unit.Dp = 10
	RadiusLG   unit.Dp = 14
	RadiusPill unit.Dp = 999
)

// Type scale. Only five sizes exist so hierarchy stays legible when a panel is
// dense with numbers.
const (
	SizeDisplay  unit.Sp = 23
	SizeTitle    unit.Sp = 17
	SizeSubtitle unit.Sp = 14
	SizeBody     unit.Sp = 13
	SizeCaption  unit.Sp = 11.5
	SizeMono     unit.Sp = 12
)

// Palette holds every colour the UI is allowed to use.
type Palette struct {
	// Surfaces, from furthest back to nearest front.
	Bg         color.NRGBA
	BgElevated color.NRGBA
	Surface    color.NRGBA
	SurfaceHi  color.NRGBA

	Border   color.NRGBA
	BorderHi color.NRGBA

	// Three text weights carry the whole information hierarchy: primary for
	// values, secondary for labels, dim for metadata.
	TextPri color.NRGBA
	TextSec color.NRGBA
	TextDim color.NRGBA

	Accent    color.NRGBA
	AccentDim color.NRGBA
	AccentFg  color.NRGBA

	OK   color.NRGBA
	Warn color.NRGBA
	Fail color.NRGBA
	Info color.NRGBA

	// Series colours for the latency chart, in assignment order. They are
	// distinguishable at 2px stroke width and stay distinct in both themes.
	Series []color.NRGBA

	// Scrim dims the app behind the loading overlay.
	Scrim color.NRGBA
}

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xFF}
}

// DarkPalette is the default. The app is a diagnostic tool that people leave
// open in the background, so it defaults to the low-glare theme.
func DarkPalette() Palette {
	return Palette{
		Bg:         rgb(0x0E1116),
		BgElevated: rgb(0x141922),
		Surface:    rgb(0x1A202B),
		SurfaceHi:  rgb(0x222A38),

		Border:   rgb(0x252E3B),
		BorderHi: rgb(0x364153),

		TextPri: rgb(0xE7EBF3),
		TextSec: rgb(0x9AA4B8),
		TextDim: rgb(0x69738A),

		Accent:    rgb(0x4C8DFF),
		AccentDim: rgb(0x27447A),
		AccentFg:  rgb(0xFFFFFF),

		OK:   rgb(0x3DCE87),
		Warn: rgb(0xF0A93B),
		Fail: rgb(0xFF6B6B),
		Info: rgb(0x8B9BFF),

		Series: []color.NRGBA{
			rgb(0x4C8DFF), rgb(0x2DD4BF), rgb(0xA78BFA), rgb(0xFBBF24),
			rgb(0xF472B6), rgb(0xA3E635), rgb(0x38BDF8), rgb(0xFB923C),
		},

		Scrim: color.NRGBA{R: 0x08, G: 0x0A, B: 0x0E, A: 0xC4},
	}
}

// LightPalette mirrors the dark one for people working in bright rooms.
func LightPalette() Palette {
	return Palette{
		Bg:         rgb(0xF6F7F9),
		BgElevated: rgb(0xFFFFFF),
		Surface:    rgb(0xFFFFFF),
		SurfaceHi:  rgb(0xF0F2F6),

		Border:   rgb(0xE3E7ED),
		BorderHi: rgb(0xCFD5DE),

		TextPri: rgb(0x111826),
		TextSec: rgb(0x4A5568),
		TextDim: rgb(0x818C9E),

		Accent:    rgb(0x2563EB),
		AccentDim: rgb(0xBFD3FA),
		AccentFg:  rgb(0xFFFFFF),

		OK:   rgb(0x0F9D58),
		Warn: rgb(0xC77700),
		Fail: rgb(0xD93636),
		Info: rgb(0x4F5DD1),

		Series: []color.NRGBA{
			rgb(0x2563EB), rgb(0x0D9488), rgb(0x7C3AED), rgb(0xD97706),
			rgb(0xDB2777), rgb(0x65A30D), rgb(0x0284C7), rgb(0xEA580C),
		},

		Scrim: color.NRGBA{R: 0x1A, G: 0x1F, B: 0x28, A: 0xB8},
	}
}

// Theme bundles the Gio material theme with this app's design tokens.
type Theme struct {
	*material.Theme
	P    Palette
	Dark bool

	// Mono is the typeface used for addresses, ports and log lines, where
	// column alignment matters more than typographic polish.
	Mono font.Typeface

	// HasCJK reports whether a font with Chinese coverage was found. When it
	// is false the UI falls back to English labels rather than rendering
	// tofu boxes.
	HasCJK bool

	// Lang selects the label set.
	Lang Lang
}

// NewTheme builds a theme from a shaper and font collection produced by
// [LoadFonts].
func NewTheme(fonts *FontSet, dark bool) *Theme {
	mt := material.NewTheme()
	mt.Shaper = text.NewShaper(text.WithCollection(fonts.Collection))
	mt.TextSize = SizeBody
	mt.Face = fonts.UI
	mt.FingerSize = 26

	th := &Theme{
		Theme:  mt,
		Dark:   dark,
		Mono:   fonts.Mono,
		HasCJK: fonts.HasCJK,
		Lang:   LangEN,
	}
	if fonts.HasCJK {
		th.Lang = LangZH
	}
	th.SetDark(dark)
	return th
}

// SetDark switches palettes and keeps the embedded material palette in sync so
// stock Gio widgets pick up the right colours too.
func (t *Theme) SetDark(dark bool) {
	t.Dark = dark
	if dark {
		t.P = DarkPalette()
	} else {
		t.P = LightPalette()
	}
	t.Theme.Palette = material.Palette{
		Bg:         t.P.Bg,
		Fg:         t.P.TextPri,
		ContrastBg: t.P.Accent,
		ContrastFg: t.P.AccentFg,
	}
}

// T looks up a localised string. It is a method on Theme so call sites stay
// short: th.T(K.Peers).
func (t *Theme) T(k Key) string { return Tr(t.Lang, k) }

// StatusColor maps a traffic-light verdict onto the palette.
func (t *Theme) StatusColor(s StatusLevel) color.NRGBA {
	switch s {
	case LevelOK:
		return t.P.OK
	case LevelWarn:
		return t.P.Warn
	case LevelFail:
		return t.P.Fail
	case LevelInfo:
		return t.P.Info
	default:
		return t.P.TextDim
	}
}

// StatusLevel is the UI-side severity, deliberately decoupled from
// netdiag.Status so widgets do not depend on the diagnostics package.
type StatusLevel int

const (
	LevelNeutral StatusLevel = iota
	LevelOK
	LevelWarn
	LevelFail
	LevelInfo
)

// SeriesColor returns a stable chart colour for index i.
func (t *Theme) SeriesColor(i int) color.NRGBA {
	if len(t.P.Series) == 0 {
		return t.P.Accent
	}
	return t.P.Series[i%len(t.P.Series)]
}

// WithAlpha returns c with its alpha scaled by a (0..1).
func WithAlpha(c color.NRGBA, a float32) color.NRGBA {
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	c.A = uint8(float32(c.A) * a)
	return c
}

// Mix blends a into b by t (0 returns a, 1 returns b).
func Mix(a, b color.NRGBA, t float32) color.NRGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	lerp := func(x, y uint8) uint8 { return uint8(float32(x) + (float32(y)-float32(x))*t) }
	return color.NRGBA{R: lerp(a.R, b.R), G: lerp(a.G, b.G), B: lerp(a.B, b.B), A: lerp(a.A, b.A)}
}
