package gui

import (
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// Short aliases, the conventional Gio shorthand.
type (
	C = layout.Context
	D = layout.Dimensions
)

// ---------------------------------------------------------------------------
// Text
// ---------------------------------------------------------------------------

// Text returns a label in the app's type scale.
func (t *Theme) Text(size unit.Sp, col color.NRGBA, txt string) material.LabelStyle {
	l := material.Label(t.Theme, size, txt)
	l.Color = col
	return l
}

// Mono returns a monospaced label, used wherever columns of addresses, ports
// or timings need to line up.
func (t *Theme) MonoLabel(size unit.Sp, col color.NRGBA, txt string) material.LabelStyle {
	l := t.Text(size, col, txt)
	l.Font.Typeface = t.Mono
	return l
}

// Title is the heading of a page.
func (t *Theme) Title(txt string) material.LabelStyle {
	l := t.Text(SizeTitle, t.P.TextPri, txt)
	l.Font.Weight = font.SemiBold
	return l
}

// Display is the single largest text on a page, used for headline numbers.
func (t *Theme) Display(txt string) material.LabelStyle {
	l := t.Text(SizeDisplay, t.P.TextPri, txt)
	l.Font.Weight = font.SemiBold
	return l
}

// Body is normal running text.
func (t *Theme) Body(txt string) material.LabelStyle {
	return t.Text(SizeBody, t.P.TextPri, txt)
}

// Secondary is a de-emphasised label, typically the left column of a key/value
// row.
func (t *Theme) Secondary(txt string) material.LabelStyle {
	return t.Text(SizeBody, t.P.TextSec, txt)
}

// Caption is metadata: timestamps, hints, units.
func (t *Theme) Caption(txt string) material.LabelStyle {
	return t.Text(SizeCaption, t.P.TextDim, txt)
}

// OneLine constrains a label to a single truncated line, which keeps table
// rows from reflowing when a peer has a long name.
func OneLine(l material.LabelStyle) material.LabelStyle {
	l.MaxLines = 1
	l.WrapPolicy = text.WrapGraphemes
	return l
}

// ---------------------------------------------------------------------------
// Primitive drawing helpers
// ---------------------------------------------------------------------------

// FillRRect paints a rounded rectangle of the given size.
func FillRRect(gtx C, size image.Point, radius unit.Dp, col color.NRGBA) {
	r := gtx.Dp(radius)
	if max := min(size.X, size.Y) / 2; r > max {
		r = max
	}
	paint.FillShape(gtx.Ops, col, clip.UniformRRect(image.Rectangle{Max: size}, r).Op(gtx.Ops))
}

// StrokeRRect outlines a rounded rectangle.
func StrokeRRect(gtx C, size image.Point, radius unit.Dp, width unit.Dp, col color.NRGBA) {
	r := gtx.Dp(radius)
	if max := min(size.X, size.Y) / 2; r > max {
		r = max
	}
	w := float32(gtx.Dp(width))
	// Inset by half the stroke width so the outline lands inside the bounds.
	inset := int(w / 2)
	rect := image.Rectangle{Min: image.Pt(inset, inset), Max: size.Sub(image.Pt(inset, inset))}
	if rect.Dx() <= 0 || rect.Dy() <= 0 {
		return
	}
	spec := clip.UniformRRect(rect, r).Path(gtx.Ops)
	paint.FillShape(gtx.Ops, col, clip.Stroke{Path: spec, Width: w}.Op())
}

// Circle paints a filled circle of the given diameter.
func Circle(gtx C, diameter int, col color.NRGBA) D {
	if diameter <= 0 {
		return D{}
	}
	r := diameter / 2
	paint.FillShape(gtx.Ops, col,
		clip.UniformRRect(image.Rectangle{Max: image.Pt(diameter, diameter)}, r).Op(gtx.Ops))
	return D{Size: image.Pt(diameter, diameter)}
}

// animFrame is the minimum gap between animation frames, i.e. a ~25fps cap.
//
// This matters more than it looks. op.InvalidateCmd with a zero At means
// "redraw immediately", so a widget that issues one every frame makes Gio
// render as fast as the machine can manage — several hundred percent CPU under
// software rendering, for a spinner nobody is watching. Scheduling the next
// frame at a fixed time bounds the loop, and concurrent animations coalesce
// onto the same wakeup.
//
// The cap alone is not enough, because a frame is not cheap: profiling this UI
// under llvmpipe put 73% of the time in Gio's path stenciler, which every
// rounded rectangle, border and icon goes through. So animation is also
// reserved for genuinely transient states — see [Theme.StatusDot]. An idle
// window must settle at zero frames per second, not a slow trickle.
const animFrame = 40 * time.Millisecond

// animSlowFrame is the cadence for ambient motion with a multi-second cycle,
// where 12fps is indistinguishable from 25 but costs half as much.
const animSlowFrame = 80 * time.Millisecond

// animate requests the next animation frame at the capped rate. Every animated
// widget in this package goes through it.
func animate(gtx C) {
	gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(animFrame)})
}

// animateSlow is [animate] for slow, decorative motion.
func animateSlow(gtx C) {
	gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(animSlowFrame)})
}

// Spacer returns a fixed-size gap.
func Spacer(v unit.Dp) layout.Spacer { return layout.Spacer{Height: v, Width: v} }

// VGap is a vertical gap.
func VGap(v unit.Dp) layout.FlexChild {
	return layout.Rigid(layout.Spacer{Height: v}.Layout)
}

// HGap is a horizontal gap.
func HGap(v unit.Dp) layout.FlexChild {
	return layout.Rigid(layout.Spacer{Width: v}.Layout)
}

// Divider draws a hairline separator.
func (t *Theme) Divider(gtx C) D {
	h := max(gtx.Dp(1), 1)
	w := gtx.Constraints.Min.X
	if w == 0 {
		w = gtx.Constraints.Max.X
	}
	paint.FillShape(gtx.Ops, t.P.Border, clip.Rect{Max: image.Pt(w, h)}.Op())
	return D{Size: image.Pt(w, h)}
}

// ---------------------------------------------------------------------------
// Card
// ---------------------------------------------------------------------------

// CardStyle is the standard container: a slightly raised surface with a
// hairline border. Cards are the only container in the UI, which is what keeps
// dense pages from turning into noise.
type CardStyle struct {
	Title    string
	Subtitle string
	// Accent tints the left edge, used to flag a section's severity without
	// adding another coloured chip.
	Accent *color.NRGBA
	// Trailing renders at the top-right of the header, for actions.
	Trailing layout.Widget
	Pad      unit.Dp
	Radius   unit.Dp
	Bg       *color.NRGBA
}

// Card returns a default card.
func (t *Theme) Card() CardStyle {
	return CardStyle{Pad: SpaceLG, Radius: RadiusMD}
}

// Layout draws the card around w.
func (c CardStyle) Layout(t *Theme, gtx C, w layout.Widget) D {
	bg := t.P.Surface
	if c.Bg != nil {
		bg = *c.Bg
	}
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			size := gtx.Constraints.Min
			FillRRect(gtx, size, c.Radius, bg)
			StrokeRRect(gtx, size, c.Radius, 1, t.P.Border)
			if c.Accent != nil {
				// A 3dp bar hugging the left edge, clipped to the card radius.
				r := gtx.Dp(c.Radius)
				defer clip.UniformRRect(image.Rectangle{Max: size}, r).Push(gtx.Ops).Pop()
				paint.FillShape(gtx.Ops, *c.Accent,
					clip.Rect{Max: image.Pt(gtx.Dp(3), size.Y)}.Op())
			}
			return D{Size: size}
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.UniformInset(c.Pad).Layout(gtx, func(gtx C) D {
				if c.Title == "" {
					return w(gtx)
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return c.header(t, gtx)
					}),
					VGap(SpaceMD),
					layout.Rigid(w),
				)
			})
		}),
	)
}

func (c CardStyle) header(t *Theme, gtx C) D {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					l := t.Text(SizeSubtitle, t.P.TextPri, c.Title)
					l.Font.Weight = font.SemiBold
					return l.Layout(gtx)
				}),
				layout.Rigid(func(gtx C) D {
					if c.Subtitle == "" {
						return D{}
					}
					return layout.Inset{Top: 2}.Layout(gtx, t.Caption(c.Subtitle).Layout)
				}),
			)
		}),
		layout.Rigid(func(gtx C) D {
			if c.Trailing == nil {
				return D{}
			}
			return c.Trailing(gtx)
		}),
	)
}

// ---------------------------------------------------------------------------
// Chips, dots, badges
// ---------------------------------------------------------------------------

// ChipStyle is a small pill carrying one piece of status.
type ChipStyle struct {
	Text  string
	Level StatusLevel
	// Solid fills the chip with the level colour instead of tinting it.
	Solid bool
	// Dot prefixes the label with a status dot.
	Dot bool
}

// Chip renders a status pill.
func (t *Theme) Chip(gtx C, s ChipStyle) D {
	fg := t.StatusColor(s.Level)
	bg := WithAlpha(fg, 0.14)
	if s.Solid {
		bg = fg
		fg = t.P.AccentFg
	}
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			FillRRect(gtx, gtx.Constraints.Min, RadiusPill, bg)
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			return layout.Inset{
				Top: 3, Bottom: 3, Left: SpaceSM, Right: SpaceSM,
			}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						if !s.Dot {
							return D{}
						}
						return layout.Inset{Right: 5}.Layout(gtx, func(gtx C) D {
							return Circle(gtx, gtx.Dp(6), fg)
						})
					}),
					layout.Rigid(OneLine(t.Text(SizeCaption, fg, s.Text)).Layout),
				)
			})
		}),
	)
}

// StatusDot draws a coloured dot; when pulse is true it breathes.
//
// Pass pulse only for states that are actually transient — connecting,
// retrying, a probe in flight. A dot that breathes forever costs a full
// redraw of the window several times a second for as long as the app is open,
// which is not a price worth paying to say "still here".
func (t *Theme) StatusDot(gtx C, level StatusLevel, pulse bool) D {
	col := t.StatusColor(level)
	d := gtx.Dp(8)
	if pulse {
		// One breath per 1.6s, derived from frame time so it stays smooth.
		phase := float64(gtx.Now.UnixNano()%int64(1600*time.Millisecond)) / float64(1600*time.Millisecond)
		a := 0.35 + 0.65*(0.5+0.5*math.Sin(phase*2*math.Pi))
		halo := WithAlpha(col, float32(a)*0.35)
		hd := gtx.Dp(16)
		off := op.Offset(image.Pt(-(hd-d)/2, -(hd-d)/2)).Push(gtx.Ops)
		Circle(gtx, hd, halo)
		off.Pop()
		animate(gtx)
	}
	return Circle(gtx, d, col)
}

// ---------------------------------------------------------------------------
// Key/value rows
// ---------------------------------------------------------------------------

// KV renders a label on the left and a value on the right. This is the primary
// way facts are shown; keeping every panel on the same row grammar is what
// makes a dense diagnostics page scannable.
type KV struct {
	Key   string
	Value string
	// Level colours the value. LevelNeutral leaves it primary-coloured.
	Level StatusLevel
	// Mono renders the value monospaced.
	Mono bool
	// Hint appears under the key in caption style.
	Hint string
	// KeyWidth fixes the label column so consecutive rows align. Zero uses a
	// flexible 40% split.
	KeyWidth unit.Dp
}

// Layout draws one key/value row.
func (t *Theme) KV(gtx C, kv KV) D {
	valCol := t.P.TextPri
	if kv.Level != LevelNeutral {
		valCol = t.StatusColor(kv.Level)
	}
	value := func(gtx C) D {
		var l material.LabelStyle
		if kv.Mono {
			l = t.MonoLabel(SizeBody, valCol, kv.Value)
		} else {
			l = t.Text(SizeBody, valCol, kv.Value)
		}
		l.Alignment = text.End
		return l.Layout(gtx)
	}
	key := func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(OneLine(t.Secondary(kv.Key)).Layout),
			layout.Rigid(func(gtx C) D {
				if kv.Hint == "" {
					return D{}
				}
				return t.Caption(kv.Hint).Layout(gtx)
			}),
		)
	}
	return layout.Inset{Top: 5, Bottom: 5}.Layout(gtx, func(gtx C) D {
		if kv.KeyWidth > 0 {
			w := gtx.Dp(kv.KeyWidth)
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					gtx.Constraints.Max.X = w
					gtx.Constraints.Min.X = w
					return key(gtx)
				}),
				HGap(SpaceMD),
				layout.Flexed(1, value),
			)
		}
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(0.42, key),
			HGap(SpaceMD),
			layout.Flexed(0.58, value),
		)
	})
}

// KVList lays out consecutive rows with hairlines between them.
func (t *Theme) KVList(gtx C, rows []KV) D {
	children := make([]layout.FlexChild, 0, len(rows)*2)
	for i, row := range rows {
		if i > 0 {
			children = append(children, layout.Rigid(t.Divider))
		}
		children = append(children, layout.Rigid(func(gtx C) D {
			return t.KV(gtx, row)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// ---------------------------------------------------------------------------
// Buttons
// ---------------------------------------------------------------------------

// ButtonKind selects a button's visual weight. A screen should have at most
// one Primary.
type ButtonKind int

const (
	ButtonPrimary ButtonKind = iota
	ButtonSubtle
	ButtonGhost
	ButtonDanger
)

// ButtonStyle is this app's button, replacing material.Button so hover, radius
// and typography match the rest of the design.
type ButtonStyle struct {
	Kind     ButtonKind
	Text     string
	Icon     IconFunc
	Disabled bool
	// Width, when non-zero, fixes the button width for aligned button rows.
	Width unit.Dp
}

// Button renders a clickable button.
func (t *Theme) Button(gtx C, click *widget.Clickable, s ButtonStyle) D {
	var bg, fg, border color.NRGBA
	switch s.Kind {
	case ButtonPrimary:
		bg, fg = t.P.Accent, t.P.AccentFg
	case ButtonDanger:
		bg, fg = t.P.Fail, t.P.AccentFg
	case ButtonSubtle:
		bg, fg, border = t.P.SurfaceHi, t.P.TextPri, t.P.Border
	default: // ghost
		bg, fg = color.NRGBA{}, t.P.TextSec
	}
	if s.Disabled {
		bg = WithAlpha(bg, 0.4)
		fg = WithAlpha(fg, 0.45)
		gtx = gtx.Disabled()
	} else if click.Hovered() {
		switch s.Kind {
		case ButtonGhost:
			bg = t.P.SurfaceHi
			fg = t.P.TextPri
		default:
			bg = Mix(bg, t.P.TextPri, 0.12)
		}
	}
	if click.Pressed() {
		bg = Mix(bg, t.P.Bg, 0.18)
	}

	return click.Layout(gtx, func(gtx C) D {
		if s.Width > 0 {
			gtx.Constraints.Min.X = gtx.Dp(s.Width)
		}
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx C) D {
				if bg.A > 0 {
					FillRRect(gtx, gtx.Constraints.Min, RadiusSM, bg)
				}
				if border.A > 0 {
					StrokeRRect(gtx, gtx.Constraints.Min, RadiusSM, 1, border)
				}
				return D{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx C) D {
				return layout.Inset{
					Top: 7, Bottom: 7, Left: SpaceMD, Right: SpaceMD,
				}.Layout(gtx, func(gtx C) D {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							if s.Icon == nil {
								return D{}
							}
							return layout.Inset{Right: 6}.Layout(gtx, func(gtx C) D {
								return s.Icon(gtx, gtx.Dp(14), fg)
							})
						}),
						layout.Rigid(func(gtx C) D {
							if s.Text == "" {
								return D{}
							}
							l := t.Text(SizeBody, fg, s.Text)
							l.Font.Weight = font.Medium
							l.Alignment = text.Middle
							return l.Layout(gtx)
						}),
					)
				})
			}),
		)
	})
}

// IconButton is a square icon-only button, used in card headers.
func (t *Theme) IconButton(gtx C, click *widget.Clickable, icon IconFunc, level StatusLevel) D {
	fg := t.P.TextSec
	if level != LevelNeutral {
		fg = t.StatusColor(level)
	}
	bg := color.NRGBA{}
	if click.Hovered() {
		bg = t.P.SurfaceHi
		if level == LevelNeutral {
			fg = t.P.TextPri
		}
	}
	return click.Layout(gtx, func(gtx C) D {
		sz := gtx.Dp(28)
		if bg.A > 0 {
			FillRRect(gtx, image.Pt(sz, sz), RadiusSM, bg)
		}
		icoSize := gtx.Dp(16)
		off := op.Offset(image.Pt((sz-icoSize)/2, (sz-icoSize)/2)).Push(gtx.Ops)
		icon(gtx, icoSize, fg)
		off.Pop()
		return D{Size: image.Pt(sz, sz)}
	})
}

// ---------------------------------------------------------------------------
// Toggle
// ---------------------------------------------------------------------------

// Toggle renders a compact switch with a label.
func (t *Theme) Toggle(gtx C, b *widget.Bool, label string) D {
	return b.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				w, h := gtx.Dp(32), gtx.Dp(18)
				track := t.P.SurfaceHi
				knobCol := t.P.TextDim
				if b.Value {
					track = t.P.Accent
					knobCol = t.P.AccentFg
				}
				FillRRect(gtx, image.Pt(w, h), RadiusPill, track)
				kd := h - gtx.Dp(4)
				kx := gtx.Dp(2)
				if b.Value {
					kx = w - kd - gtx.Dp(2)
				}
				off := op.Offset(image.Pt(kx, gtx.Dp(2))).Push(gtx.Ops)
				Circle(gtx, kd, knobCol)
				off.Pop()
				return D{Size: image.Pt(w, h)}
			}),
			layout.Rigid(func(gtx C) D {
				if label == "" {
					return D{}
				}
				return layout.Inset{Left: SpaceSM}.Layout(gtx, t.Secondary(label).Layout)
			}),
		)
	})
}

// ---------------------------------------------------------------------------
// Segmented control (used for level/language/theme pickers)
// ---------------------------------------------------------------------------

// SegmentOption is one choice in a segmented control.
type SegmentOption struct {
	Key   string
	Label string
	// Count, when non-negative, is shown as a trailing tally.
	Count int
	Level StatusLevel
}

// Segmented renders a row of mutually exclusive options backed by a
// widget.Enum.
func (t *Theme) Segmented(gtx C, e *widget.Enum, opts []SegmentOption) D {
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			FillRRect(gtx, gtx.Constraints.Min, RadiusSM, t.P.BgElevated)
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			return layout.UniformInset(3).Layout(gtx, func(gtx C) D {
				children := make([]layout.FlexChild, 0, len(opts))
				for _, o := range opts {
					children = append(children, layout.Rigid(func(gtx C) D {
						return t.segment(gtx, e, o)
					}))
				}
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx, children...)
			})
		}),
	)
}

func (t *Theme) segment(gtx C, e *widget.Enum, o SegmentOption) D {
	selected := e.Value == o.Key
	fg := t.P.TextSec
	if selected {
		fg = t.P.TextPri
	}
	if o.Level != LevelNeutral && selected {
		fg = t.StatusColor(o.Level)
	}
	return e.Layout(gtx, o.Key, func(gtx C) D {
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx C) D {
				if selected {
					FillRRect(gtx, gtx.Constraints.Min, RadiusSM-2, t.P.SurfaceHi)
				}
				return D{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx C) D {
				return layout.Inset{Top: 4, Bottom: 4, Left: SpaceMD, Right: SpaceMD}.Layout(gtx, func(gtx C) D {
					label := o.Label
					if o.Count >= 0 {
						label = o.Label + "  " + itoa(o.Count)
					}
					l := t.Text(SizeCaption, fg, label)
					if selected {
						l.Font.Weight = font.Medium
					}
					return l.Layout(gtx)
				})
			}),
		)
	})
}

// ---------------------------------------------------------------------------
// Empty state
// ---------------------------------------------------------------------------

// EmptyState is what a panel shows instead of a blank area. It always says why
// the area is empty, never just "no data".
func (t *Theme) EmptyState(gtx C, icon IconFunc, title, hint string) D {
	return layout.Center.Layout(gtx, func(gtx C) D {
		return layout.Inset{Top: Space2XL, Bottom: Space2XL}.Layout(gtx, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					if icon == nil {
						return D{}
					}
					return icon(gtx, gtx.Dp(28), WithAlpha(t.P.TextDim, 0.7))
				}),
				VGap(SpaceMD),
				layout.Rigid(func(gtx C) D {
					l := t.Text(SizeBody, t.P.TextSec, title)
					l.Alignment = text.Middle
					return l.Layout(gtx)
				}),
				layout.Rigid(func(gtx C) D {
					if hint == "" {
						return D{}
					}
					return layout.Inset{Top: SpaceXS}.Layout(gtx, func(gtx C) D {
						l := t.Caption(hint)
						l.Alignment = text.Middle
						return l.Layout(gtx)
					})
				}),
			)
		})
	})
}

// ---------------------------------------------------------------------------
// Spinner
// ---------------------------------------------------------------------------

// Spinner draws an indeterminate arc. It requests the next frame itself, so
// callers just place it.
func (t *Theme) Spinner(gtx C, size int, col color.NRGBA) D {
	if size <= 0 {
		size = gtx.Dp(20)
	}
	const period = 1100 * time.Millisecond
	phase := float32(gtx.Now.UnixNano()%int64(period)) / float32(period)

	stroke := float32(gtx.Dp(2))
	r := float32(size)/2 - stroke/2
	center := f32.Pt(float32(size)/2, float32(size)/2)

	// Track.
	drawArc(gtx, center, r, stroke, 0, 2*math.Pi, WithAlpha(col, 0.15))
	// Sweep: the arc length breathes so the motion reads as progress rather
	// than a rotating stick.
	sweep := float32(0.25*math.Pi) + float32(1.2*math.Pi)*(0.5+0.5*float32(math.Sin(float64(phase)*2*math.Pi)))
	start := phase * 2 * math.Pi * 2
	drawArc(gtx, center, r, stroke, start, sweep, col)

	animate(gtx)
	return D{Size: image.Pt(size, size)}
}

// drawArc strokes an arc of `sweep` radians starting at `start`.
func drawArc(gtx C, center f32.Point, radius, width, start, sweep float32, col color.NRGBA) {
	if radius <= 0 || sweep <= 0 {
		return
	}
	var p clip.Path
	p.Begin(gtx.Ops)
	begin := f32.Pt(
		center.X+radius*float32(math.Cos(float64(start))),
		center.Y+radius*float32(math.Sin(float64(start))),
	)
	p.MoveTo(begin)
	// clip.Path.Arc rotates the pen around the focus points; for a circle both
	// foci are the centre.
	p.Arc(center.Sub(begin), center.Sub(begin), sweep)
	paint.FillShape(gtx.Ops, col, clip.Stroke{Path: p.End(), Width: width}.Op())
}

// ProgressBar draws a determinate bar in [0,1].
func (t *Theme) ProgressBar(gtx C, progress float32, col color.NRGBA) D {
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	w := gtx.Constraints.Max.X
	h := gtx.Dp(4)
	FillRRect(gtx, image.Pt(w, h), RadiusPill, WithAlpha(col, 0.16))
	fw := int(float32(w) * progress)
	if fw > 0 {
		FillRRect(gtx, image.Pt(fw, h), RadiusPill, col)
	}
	return D{Size: image.Pt(w, h)}
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// FormatLatency renders a duration the way a network tool should: sub-10ms
// gets one decimal, everything else is a whole number of milliseconds.
func FormatLatency(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	ms := float64(d) / float64(time.Millisecond)
	switch {
	case ms < 10:
		return trimZero(ms, 1) + " ms"
	case ms < 1000:
		return itoa(int(ms+0.5)) + " ms"
	default:
		return trimZero(ms/1000, 2) + " s"
	}
}

func trimZero(v float64, prec int) string {
	mult := math.Pow(10, float64(prec))
	v = math.Round(v*mult) / mult
	s := strconvFormat(v, prec)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// strconvFormat avoids importing strconv just for one call site pattern; it
// formats with a fixed number of decimals.
func strconvFormat(v float64, prec int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	mult := math.Pow(10, float64(prec))
	scaled := int64(math.Round(v * mult))
	intPart := scaled / int64(mult)
	frac := scaled % int64(mult)
	s := itoa(int(intPart))
	if prec > 0 {
		fs := itoa(int(frac))
		for len(fs) < prec {
			fs = "0" + fs
		}
		s += "." + fs
	}
	if neg {
		s = "-" + s
	}
	return s
}

// FormatBytes renders a byte count with binary units.
func FormatBytes(n int64) string {
	if n < 0 {
		return "—"
	}
	const unit = 1024
	if n < unit {
		return itoa(int(n)) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	return trimZero(float64(n)/float64(div), 1) + " " + suffixes[exp]
}

// FormatDuration renders an uptime-style duration.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h >= 24:
		return itoa(h/24) + "d " + itoa(h%24) + "h"
	case h > 0:
		return itoa(h) + "h " + itoa(m) + "m"
	case m > 0:
		return itoa(m) + "m " + itoa(s) + "s"
	default:
		return itoa(s) + "s"
	}
}

// RelTime renders how long ago t was, localised.
func RelTime(th *Theme, t time.Time, now time.Time) string {
	if t.IsZero() {
		return th.T(KNever)
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return th.T(KJustNow)
	case d < 5*time.Second:
		return th.T(KJustNow)
	case d < time.Minute:
		return itoa(int(d.Seconds())) + th.T(KSecondsAgo)
	case d < time.Hour:
		return itoa(int(d.Minutes())) + th.T(KMinutesAgo)
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + th.T(KHoursAgo)
	default:
		return t.Format("01-02 15:04")
	}
}

// Truncate shortens s to at most n runes, appending an ellipsis.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
