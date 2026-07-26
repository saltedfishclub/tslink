package gui

import (
	"image"
	"image/color"
	"math"
	"time"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
)

// ChartPoint is one sample. A point with OK false is a failed probe: the line
// breaks there rather than being interpolated across, because pretending a
// dropped ping was a slow one hides exactly the problem the user opened this
// panel to find.
type ChartPoint struct {
	At    time.Time
	Value float64 // milliseconds
	OK    bool
}

// ChartSeries is one line on the chart.
type ChartSeries struct {
	Name   string
	Color  color.NRGBA
	Points []ChartPoint
	Hidden bool
	// Subtitle appears under the name in the legend, typically the peer's route.
	Subtitle string
}

// ChartStyle configures the plot.
type ChartStyle struct {
	Height unit.Dp
	// MaxWindow caps how far back the x axis reaches. The axis is scaled to the
	// data's own extent and only clamped by this, so the plot fills its width
	// from the second sample onward instead of leaving the first N minutes of
	// the window blank while history accumulates.
	MaxWindow time.Duration
	// Now is the wall clock, used only as a fallback when there is no data.
	Now time.Time
	// Unit labels the y axis.
	Unit string
	// FillSingle draws a soft gradient under the line when exactly one series
	// is visible, which reads better than a lone stroke on a big canvas.
	FillSingle bool
}

// Chart is the stateful part of the plot: which point the pointer is near.
type Chart struct {
	hover    f32.Point
	hovering bool
	// plot is the last plotted rectangle, used to map hover x back to a time.
	plot image.Rectangle
	// tMin/tMax are the x domain resolved by the last Layout. HoverIndex maps
	// the pointer through these rather than recomputing from ChartStyle, so the
	// crosshair cannot disagree with the drawn line.
	tMin, tMax time.Time
}

// minPlotSpan keeps the axis sane when every visible sample shares a timestamp,
// which happens on the very first frame after a refresh.
const minPlotSpan = 10 * time.Second

// domain resolves the x axis from the visible data, clamped to st.MaxWindow.
func domain(series []ChartSeries, st ChartStyle) (tMin, tMax time.Time) {
	now := st.Now
	if now.IsZero() {
		now = time.Now()
	}
	window := st.MaxWindow
	if window <= 0 {
		window = 3 * time.Minute
	}

	var first, last time.Time
	for _, s := range series {
		if s.Hidden {
			continue
		}
		for _, p := range s.Points {
			if first.IsZero() || p.At.Before(first) {
				first = p.At
			}
			if last.IsZero() || p.At.After(last) {
				last = p.At
			}
		}
	}
	if first.IsZero() {
		return now.Add(-window), now
	}
	// Never show more than the window, however much history is retained.
	if last.Sub(first) > window {
		first = last.Add(-window)
	}
	if last.Sub(first) < minPlotSpan {
		first = last.Add(-minPlotSpan)
	}
	return first, last
}

// HoverIndex returns the sample index the pointer is nearest within s, or -1.
func (c *Chart) HoverIndex(series ChartSeries) int {
	if !c.hovering || len(series.Points) == 0 || c.plot.Dx() <= 0 {
		return -1
	}
	span := c.tMax.Sub(c.tMin)
	if span <= 0 {
		return -1
	}
	frac := float64(c.hover.X-float32(c.plot.Min.X)) / float64(c.plot.Dx())
	if frac < 0 || frac > 1 {
		return -1
	}
	target := c.tMin.Add(time.Duration(frac * float64(span)))
	best, bestDelta := -1, time.Duration(math.MaxInt64)
	for i, p := range series.Points {
		d := p.At.Sub(target)
		if d < 0 {
			d = -d
		}
		if d < bestDelta {
			best, bestDelta = i, d
		}
	}
	// Only report a match when the nearest sample is genuinely close, so the
	// crosshair does not snap to a distant point in a sparse series.
	if bestDelta > span/20 {
		return -1
	}
	return best
}

// Layout draws the chart.
func (c *Chart) Layout(t *Theme, gtx C, st ChartStyle, series []ChartSeries) D {
	if st.Now.IsZero() {
		st.Now = gtx.Now
	}
	h := gtx.Dp(st.Height)
	if h <= 0 {
		h = gtx.Dp(180)
	}
	w := gtx.Constraints.Max.X
	size := image.Pt(w, h)

	gutterL := gtx.Dp(44)
	gutterB := gtx.Dp(18)
	plot := image.Rect(gutterL, gtx.Dp(6), w-gtx.Dp(6), h-gutterB)
	c.plot = plot
	if plot.Dx() <= 0 || plot.Dy() <= 0 {
		return D{Size: size}
	}

	// Pointer tracking over the plot area.
	c.update(gtx, size)

	yMax := niceMax(maxVisible(series))
	c.tMin, c.tMax = domain(series, st)

	c.drawGrid(t, gtx, plot, yMax, c.tMax.Sub(c.tMin))
	for _, s := range series {
		if s.Hidden || len(s.Points) == 0 {
			continue
		}
		c.drawSeries(t, gtx, plot, s, c.tMin, c.tMax, yMax, st.FillSingle && visibleCount(series) == 1)
	}
	c.drawCrosshair(t, gtx, plot, series, yMax)

	return D{Size: size}
}

func (c *Chart) update(gtx C, size image.Point) {
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	event.Op(gtx.Ops, c)
	for {
		ev, ok := gtx.Event(pointer.Filter{
			Target: c,
			Kinds:  pointer.Move | pointer.Enter | pointer.Leave | pointer.Drag,
		})
		if !ok {
			break
		}
		pe, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch pe.Kind {
		case pointer.Leave, pointer.Cancel:
			c.hovering = false
		default:
			c.hovering = true
			c.hover = pe.Position
		}
	}
}

func maxVisible(series []ChartSeries) float64 {
	m := 0.0
	for _, s := range series {
		if s.Hidden {
			continue
		}
		for _, p := range s.Points {
			if p.OK && p.Value > m {
				m = p.Value
			}
		}
	}
	return m
}

func visibleCount(series []ChartSeries) int {
	n := 0
	for _, s := range series {
		if !s.Hidden && len(s.Points) > 0 {
			n++
		}
	}
	return n
}

// niceMax rounds an axis maximum up to a 1/2/5 x 10^n step so the gridlines
// land on numbers a human reads without effort.
func niceMax(v float64) float64 {
	if v <= 0 {
		return 50
	}
	v *= 1.15 // headroom so the peak is not glued to the top edge
	exp := math.Floor(math.Log10(v))
	base := math.Pow(10, exp)
	switch f := v / base; {
	case f <= 1:
		return base
	case f <= 2:
		return 2 * base
	case f <= 5:
		return 5 * base
	default:
		return 10 * base
	}
}

func (c *Chart) drawGrid(t *Theme, gtx C, plot image.Rectangle, yMax float64, span time.Duration) {
	const rows = 4
	lineCol := WithAlpha(t.P.Border, 0.9)
	for i := 0; i <= rows; i++ {
		frac := float64(i) / rows
		y := plot.Max.Y - int(frac*float64(plot.Dy()))
		paint.FillShape(gtx.Ops, lineCol, clip.Rect{
			Min: image.Pt(plot.Min.X, y),
			Max: image.Pt(plot.Max.X, y+1),
		}.Op())

		val := frac * yMax
		lbl := t.MonoLabel(SizeCaption, t.P.TextDim, trimZero(val, 0))
		lbl.Alignment = text.End
		off := op.Offset(image.Pt(0, y-gtx.Dp(7))).Push(gtx.Ops)
		lgtx := gtx
		lgtx.Constraints.Max.X = plot.Min.X - gtx.Dp(6)
		lgtx.Constraints.Min.X = lgtx.Constraints.Max.X
		lbl.Layout(lgtx)
		off.Pop()
	}

	// X axis: three labels, oldest to newest.
	labels := []struct {
		frac float64
		txt  string
	}{
		{0, "-" + FormatDuration(span)},
		{0.5, "-" + FormatDuration(span/2)},
		{1, "now"},
	}
	if t.Lang == LangZH {
		labels[2].txt = "现在"
	}
	for _, l := range labels {
		x := plot.Min.X + int(l.frac*float64(plot.Dx()))
		lbl := t.Text(SizeCaption, t.P.TextDim, l.txt)
		switch {
		case l.frac == 0:
			lbl.Alignment = text.Start
		case l.frac == 1:
			lbl.Alignment = text.End
		default:
			lbl.Alignment = text.Middle
		}
		wide := gtx.Dp(70)
		ox := x - wide/2
		if l.frac == 0 {
			ox = x
		}
		if l.frac == 1 {
			ox = x - wide
		}
		off := op.Offset(image.Pt(ox, plot.Max.Y+gtx.Dp(3))).Push(gtx.Ops)
		lgtx := gtx
		lgtx.Constraints.Max.X = wide
		lgtx.Constraints.Min.X = wide
		lbl.Layout(lgtx)
		off.Pop()
	}
}

// pos maps a sample onto plot coordinates.
func pos(plot image.Rectangle, tMin, tMax time.Time, yMax float64, p ChartPoint) f32.Point {
	span := tMax.Sub(tMin)
	if span <= 0 {
		span = time.Second
	}
	fx := float64(p.At.Sub(tMin)) / float64(span)
	fx = math.Max(0, math.Min(1, fx))
	fy := p.Value / yMax
	fy = math.Max(0, math.Min(1, fy))
	return f32.Pt(
		float32(plot.Min.X)+float32(fx)*float32(plot.Dx()),
		float32(plot.Max.Y)-float32(fy)*float32(plot.Dy()),
	)
}

func (c *Chart) drawSeries(t *Theme, gtx C, plot image.Rectangle, s ChartSeries, tMin, tMax time.Time, yMax float64, fill bool) {
	defer clip.Rect(plot).Push(gtx.Ops).Pop()

	// Optional area fill, drawn first so the stroke sits on top.
	if fill {
		var ap clip.Path
		ap.Begin(gtx.Ops)
		started := false
		var lastX float32
		for _, p := range s.Points {
			if !p.OK {
				continue
			}
			pt := pos(plot, tMin, tMax, yMax, p)
			if !started {
				ap.MoveTo(f32.Pt(pt.X, float32(plot.Max.Y)))
				ap.LineTo(pt)
				started = true
			} else {
				ap.LineTo(pt)
			}
			lastX = pt.X
		}
		if started {
			ap.LineTo(f32.Pt(lastX, float32(plot.Max.Y)))
			ap.Close()
			paint.FillShape(gtx.Ops, WithAlpha(s.Color, 0.13), clip.Outline{Path: ap.End()}.Op())
		}
	}

	var p clip.Path
	p.Begin(gtx.Ops)
	pen := false
	for _, sp := range s.Points {
		if !sp.OK {
			pen = false // break the line across a dropped probe
			continue
		}
		pt := pos(plot, tMin, tMax, yMax, sp)
		if !pen {
			p.MoveTo(pt)
			pen = true
		} else {
			p.LineTo(pt)
		}
	}
	paint.FillShape(gtx.Ops, s.Color,
		clip.Stroke{Path: p.End(), Width: float32(gtx.Dp(1.6))}.Op())

	// Mark failures with a small tick on the baseline so loss is visible even
	// when the surrounding samples are fine.
	for _, sp := range s.Points {
		if sp.OK {
			continue
		}
		pt := pos(plot, tMin, tMax, yMax, ChartPoint{At: sp.At, Value: 0, OK: true})
		x := int(pt.X)
		paint.FillShape(gtx.Ops, WithAlpha(t.P.Fail, 0.75), clip.Rect{
			Min: image.Pt(x, plot.Max.Y-gtx.Dp(5)),
			Max: image.Pt(x+max(gtx.Dp(1.5), 1), plot.Max.Y),
		}.Op())
	}

	// A dot on the most recent successful sample anchors the eye to "now".
	for i := len(s.Points) - 1; i >= 0; i-- {
		if !s.Points[i].OK {
			continue
		}
		pt := pos(plot, tMin, tMax, yMax, s.Points[i])
		d := gtx.Dp(5)
		off := op.Offset(image.Pt(int(pt.X)-d/2, int(pt.Y)-d/2)).Push(gtx.Ops)
		Circle(gtx, d, s.Color)
		off.Pop()
		break
	}
}

func (c *Chart) drawCrosshair(t *Theme, gtx C, plot image.Rectangle, series []ChartSeries, yMax float64) {
	if !c.hovering {
		return
	}
	x := int(c.hover.X)
	if x < plot.Min.X || x > plot.Max.X {
		return
	}
	paint.FillShape(gtx.Ops, WithAlpha(t.P.TextDim, 0.5), clip.Rect{
		Min: image.Pt(x, plot.Min.Y),
		Max: image.Pt(x+1, plot.Max.Y),
	}.Op())

	for _, s := range series {
		if s.Hidden {
			continue
		}
		i := c.HoverIndex(s)
		if i < 0 || !s.Points[i].OK {
			continue
		}
		pt := pos(plot, c.tMin, c.tMax, yMax, s.Points[i])
		d := gtx.Dp(7)
		off := op.Offset(image.Pt(int(pt.X)-d/2, int(pt.Y)-d/2)).Push(gtx.Ops)
		Circle(gtx, d, s.Color)
		inner := gtx.Dp(3)
		off2 := op.Offset(image.Pt((d-inner)/2, (d-inner)/2)).Push(gtx.Ops)
		Circle(gtx, inner, t.P.Bg)
		off2.Pop()
		off.Pop()
	}
}

// ---------------------------------------------------------------------------
// Legend
// ---------------------------------------------------------------------------

// LegendEntry is one row of the chart legend.
type LegendEntry struct {
	Name     string
	Subtitle string
	Color    color.NRGBA
	Value    string
	Hidden   bool
}

// Legend renders the chart legend as a wrapping row of toggles. The caller
// supplies a clickable per entry so hiding a noisy peer is one click away.
//
// Wrapping matters here: with the eight series the chart allows, the chips are
// far wider than the card, and a plain Flex would silently clip the trailing
// ones — the peers you could no longer toggle were exactly the ones you could
// no longer identify.
func (t *Theme) Legend(gtx C, entries []LegendEntry, click func(i int) layout.Widget) D {
	if len(entries) == 0 {
		return D{}
	}
	children := make([]layout.Widget, 0, len(entries))
	for i := range entries {
		children = append(children, click(i))
	}
	return WrapRow(gtx, 0, children)
}

// LegendChip draws one legend entry.
func (t *Theme) LegendChip(gtx C, e LegendEntry, hovered bool) D {
	fg := t.P.TextSec
	swatch := e.Color
	if e.Hidden {
		fg = WithAlpha(t.P.TextDim, 0.7)
		swatch = WithAlpha(e.Color, 0.3)
	}
	if hovered {
		fg = t.P.TextPri
	}
	return layout.Inset{Right: SpaceMD, Top: 3, Bottom: 3}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return layout.Inset{Right: 6}.Layout(gtx, func(gtx C) D {
					h := gtx.Dp(3)
					w := gtx.Dp(12)
					FillRRect(gtx, image.Pt(w, h), RadiusPill, swatch)
					return D{Size: image.Pt(w, h)}
				})
			}),
			// Bounded: peer names can be long, and one runaway chip would push
			// every following one onto its own line.
			layout.Rigid(OneLine(t.Text(SizeCaption, fg, Truncate(e.Name, 22))).Layout),
			layout.Rigid(func(gtx C) D {
				if e.Value == "" {
					return D{}
				}
				return layout.Inset{Left: 5}.Layout(gtx,
					t.MonoLabel(SizeCaption, WithAlpha(fg, 0.8), e.Value).Layout)
			}),
		)
	})
}

// ---------------------------------------------------------------------------
// Sparkline
// ---------------------------------------------------------------------------

// Sparkline draws a compact latency trace for a table row: no axes, no labels,
// just the shape of the last few minutes.
func (t *Theme) Sparkline(gtx C, points []ChartPoint, col color.NRGBA, w, h unit.Dp) D {
	width, height := gtx.Dp(w), gtx.Dp(h)
	size := image.Pt(width, height)
	if len(points) < 2 || width <= 0 || height <= 0 {
		// A flat hairline is a clearer "no data yet" than empty space.
		paint.FillShape(gtx.Ops, WithAlpha(t.P.Border, 0.8), clip.Rect{
			Min: image.Pt(0, height/2),
			Max: image.Pt(width, height/2+1),
		}.Op())
		return D{Size: size}
	}

	yMax := 0.0
	for _, p := range points {
		if p.OK && p.Value > yMax {
			yMax = p.Value
		}
	}
	if yMax <= 0 {
		yMax = 1
	}
	yMax *= 1.2

	plot := image.Rect(0, 1, width, height-1)
	tMin, tMax := points[0].At, points[len(points)-1].At
	if !tMax.After(tMin) {
		tMax = tMin.Add(time.Second)
	}

	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	var p clip.Path
	p.Begin(gtx.Ops)
	pen := false
	for _, sp := range points {
		if !sp.OK {
			pen = false
			continue
		}
		pt := pos(plot, tMin, tMax, yMax, sp)
		if !pen {
			p.MoveTo(pt)
			pen = true
		} else {
			p.LineTo(pt)
		}
	}
	paint.FillShape(gtx.Ops, col, clip.Stroke{Path: p.End(), Width: float32(gtx.Dp(1.3))}.Op())

	for _, sp := range points {
		if sp.OK {
			continue
		}
		pt := pos(plot, tMin, tMax, yMax, ChartPoint{At: sp.At, Value: 0, OK: true})
		x := int(pt.X)
		paint.FillShape(gtx.Ops, WithAlpha(t.P.Fail, 0.8), clip.Rect{
			Min: image.Pt(x, plot.Max.Y-gtx.Dp(3)),
			Max: image.Pt(x+1, plot.Max.Y),
		}.Op())
	}
	return D{Size: size}
}
