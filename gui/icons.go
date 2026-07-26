package gui

import (
	"image"
	"image/color"
	"math"

	"gioui.org/f32"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// IconFunc draws an icon of the given pixel size in col, occupying a square of
// that size.
//
// The icons are drawn as vector line art rather than pulled from an icon font
// or the shiny material set: a dozen hand-drawn paths keep the binary small,
// avoid a dependency, and let every glyph share one stroke weight so the
// toolbar reads as a set.
type IconFunc func(gtx C, size int, col color.NRGBA) D

// defaultStroke is the icon stroke width as a fraction of the icon box.
const defaultStroke = 0.085

// iconCanvas sets up a unit coordinate space (0..1 in both axes) and strokes
// whatever the draw function puts on the path.
func iconCanvas(gtx C, size int, col color.NRGBA, width float32, draw func(p *clip.Path, pt func(x, y float32) f32.Point)) D {
	if size <= 0 {
		return D{}
	}
	s := float32(size)
	pt := func(x, y float32) f32.Point { return f32.Pt(x*s, y*s) }

	var p clip.Path
	p.Begin(gtx.Ops)
	draw(&p, pt)

	w := width * s
	if w < 1 {
		w = 1
	}
	paint.FillShape(gtx.Ops, col, clip.Stroke{Path: p.End(), Width: w}.Op())
	return D{Size: image.Pt(size, size)}
}

// arcAt appends a circle (or arc) centred at (cx, cy) with radius r, in unit
// coordinates.
func arcAt(p *clip.Path, pt func(x, y float32) f32.Point, cx, cy, r, startAngle, sweep float32) {
	start := pt(
		cx+r*float32(math.Cos(float64(startAngle))),
		cy+r*float32(math.Sin(float64(startAngle))),
	)
	c := pt(cx, cy)
	p.MoveTo(start)
	d := c.Sub(start)
	p.Arc(d, d, sweep)
}

func poly(p *clip.Path, pt func(x, y float32) f32.Point, pts ...[2]float32) {
	if len(pts) == 0 {
		return
	}
	p.MoveTo(pt(pts[0][0], pts[0][1]))
	for _, q := range pts[1:] {
		p.LineTo(pt(q[0], q[1]))
	}
}

func line(p *clip.Path, pt func(x, y float32) f32.Point, x1, y1, x2, y2 float32) {
	p.MoveTo(pt(x1, y1))
	p.LineTo(pt(x2, y2))
}

func rect(p *clip.Path, pt func(x, y float32) f32.Point, x, y, w, h float32) {
	p.MoveTo(pt(x, y))
	p.LineTo(pt(x+w, y))
	p.LineTo(pt(x+w, y+h))
	p.LineTo(pt(x, y+h))
	p.Close()
}

// dot paints a filled circle in unit coordinates, for icons that need a solid
// node rather than an outline.
func dot(gtx C, size int, col color.NRGBA, cx, cy, r float32) {
	s := float32(size)
	d := int(2 * r * s)
	if d < 2 {
		d = 2
	}
	off := op.Offset(image.Pt(int(cx*s)-d/2, int(cy*s)-d/2)).Push(gtx.Ops)
	Circle(gtx, d, col)
	off.Pop()
}

// ---------------------------------------------------------------------------
// Navigation icons
// ---------------------------------------------------------------------------

// IconGrid is the overview page: four panes.
func IconGrid(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		rect(p, pt, 0.14, 0.14, 0.30, 0.30)
		rect(p, pt, 0.56, 0.14, 0.30, 0.30)
		rect(p, pt, 0.14, 0.56, 0.30, 0.30)
		rect(p, pt, 0.56, 0.56, 0.30, 0.30)
	})
}

// IconNodes is the peers page: three linked nodes.
func IconNodes(gtx C, size int, col color.NRGBA) D {
	d := iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		line(p, pt, 0.50, 0.24, 0.22, 0.72)
		line(p, pt, 0.50, 0.24, 0.78, 0.72)
		line(p, pt, 0.22, 0.72, 0.78, 0.72)
	})
	dot(gtx, size, col, 0.50, 0.22, 0.13)
	dot(gtx, size, col, 0.21, 0.75, 0.13)
	dot(gtx, size, col, 0.79, 0.75, 0.13)
	return d
}

// IconBroadcast is the LAN page: a source radiating outwards.
func IconBroadcast(gtx C, size int, col color.NRGBA) D {
	d := iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		const q = math.Pi / 4
		arcAt(p, pt, 0.5, 0.5, 0.22, -q, 2*q)
		arcAt(p, pt, 0.5, 0.5, 0.40, -q, 2*q)
		arcAt(p, pt, 0.5, 0.5, 0.22, float32(math.Pi)-q, 2*q)
		arcAt(p, pt, 0.5, 0.5, 0.40, float32(math.Pi)-q, 2*q)
	})
	dot(gtx, size, col, 0.5, 0.5, 0.12)
	return d
}

// IconPulse is the diagnostics page: an activity trace.
func IconPulse(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		poly(p, pt,
			[2]float32{0.08, 0.52},
			[2]float32{0.28, 0.52},
			[2]float32{0.40, 0.22},
			[2]float32{0.56, 0.80},
			[2]float32{0.68, 0.52},
			[2]float32{0.92, 0.52},
		)
	})
}

// IconList is the logs page.
func IconList(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		line(p, pt, 0.16, 0.28, 0.84, 0.28)
		line(p, pt, 0.16, 0.50, 0.84, 0.50)
		line(p, pt, 0.16, 0.72, 0.60, 0.72)
	})
}

// IconSliders is the settings page.
func IconSliders(gtx C, size int, col color.NRGBA) D {
	d := iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		line(p, pt, 0.12, 0.30, 0.88, 0.30)
		line(p, pt, 0.12, 0.70, 0.88, 0.70)
	})
	dot(gtx, size, col, 0.34, 0.30, 0.13)
	dot(gtx, size, col, 0.66, 0.70, 0.13)
	return d
}

// ---------------------------------------------------------------------------
// Action icons
// ---------------------------------------------------------------------------

// IconCopy is the copy-to-clipboard action.
func IconCopy(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		rect(p, pt, 0.32, 0.32, 0.54, 0.54)
		poly(p, pt,
			[2]float32{0.68, 0.20},
			[2]float32{0.14, 0.20},
			[2]float32{0.14, 0.68},
		)
	})
}

// IconUpload is the share/upload action.
func IconUpload(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		line(p, pt, 0.5, 0.16, 0.5, 0.64)
		poly(p, pt,
			[2]float32{0.30, 0.36},
			[2]float32{0.50, 0.16},
			[2]float32{0.70, 0.36},
		)
		poly(p, pt,
			[2]float32{0.16, 0.62},
			[2]float32{0.16, 0.86},
			[2]float32{0.84, 0.86},
			[2]float32{0.84, 0.62},
		)
	})
}

// IconSave is the write-to-disk action.
func IconSave(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		line(p, pt, 0.5, 0.14, 0.5, 0.62)
		poly(p, pt,
			[2]float32{0.30, 0.42},
			[2]float32{0.50, 0.62},
			[2]float32{0.70, 0.42},
		)
		poly(p, pt,
			[2]float32{0.16, 0.62},
			[2]float32{0.16, 0.86},
			[2]float32{0.84, 0.86},
			[2]float32{0.84, 0.62},
		)
	})
}

// IconRefresh is the re-run action.
func IconRefresh(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		arcAt(p, pt, 0.5, 0.5, 0.32, -1.9, 4.9)
		poly(p, pt,
			[2]float32{0.60, 0.06},
			[2]float32{0.61, 0.30},
			[2]float32{0.38, 0.24},
		)
	})
}

// IconCheck marks a passed check.
func IconCheck(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, 0.11, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		poly(p, pt,
			[2]float32{0.18, 0.52},
			[2]float32{0.42, 0.74},
			[2]float32{0.82, 0.28},
		)
	})
}

// IconCross marks a failed or unsupported check. It pairs with [IconCheck] at
// the same stroke weight so a row mixing the two reads as one set.
func IconCross(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, 0.11, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		line(p, pt, 0.24, 0.24, 0.76, 0.76)
		line(p, pt, 0.76, 0.24, 0.24, 0.76)
	})
}

// IconDash marks a check whose answer is unknown, as distinct from a "no".
func IconDash(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, 0.11, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		line(p, pt, 0.22, 0.5, 0.78, 0.5)
	})
}

// IconWarn marks a warning.
func IconWarn(gtx C, size int, col color.NRGBA) D {
	d := iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		poly(p, pt,
			[2]float32{0.50, 0.12},
			[2]float32{0.92, 0.84},
			[2]float32{0.08, 0.84},
		)
		p.Close()
		line(p, pt, 0.5, 0.40, 0.5, 0.60)
	})
	dot(gtx, size, col, 0.5, 0.72, 0.055)
	return d
}

// IconChevronRight indicates an expandable row.
func IconChevronRight(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		poly(p, pt,
			[2]float32{0.40, 0.24},
			[2]float32{0.66, 0.50},
			[2]float32{0.40, 0.76},
		)
	})
}

// IconChevronDown indicates an expanded row.
func IconChevronDown(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		poly(p, pt,
			[2]float32{0.24, 0.40},
			[2]float32{0.50, 0.66},
			[2]float32{0.76, 0.40},
		)
	})
}

// IconSearch prefixes the log filter field.
func IconSearch(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		arcAt(p, pt, 0.44, 0.44, 0.28, 0, 2*math.Pi)
		line(p, pt, 0.64, 0.64, 0.86, 0.86)
	})
}

// IconGlobe marks anything about the public internet.
func IconGlobe(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		arcAt(p, pt, 0.5, 0.5, 0.38, 0, 2*math.Pi)
		line(p, pt, 0.12, 0.5, 0.88, 0.5)
		// Two meridians, drawn as opposing quadratic bows.
		p.MoveTo(pt(0.5, 0.12))
		p.QuadTo(pt(0.22, 0.5), pt(0.5, 0.88))
		p.MoveTo(pt(0.5, 0.12))
		p.QuadTo(pt(0.78, 0.5), pt(0.5, 0.88))
	})
}

// IconServer marks a discovered game server.
func IconServer(gtx C, size int, col color.NRGBA) D {
	d := iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		rect(p, pt, 0.14, 0.18, 0.72, 0.26)
		rect(p, pt, 0.14, 0.56, 0.72, 0.26)
	})
	dot(gtx, size, col, 0.26, 0.31, 0.05)
	dot(gtx, size, col, 0.26, 0.69, 0.05)
	return d
}

// IconLink marks a peer referenced by a config rule.
func IconLink(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		arcAt(p, pt, 0.34, 0.66, 0.22, -2.36, 3.14)
		arcAt(p, pt, 0.66, 0.34, 0.22, 0.78, 3.14)
		line(p, pt, 0.38, 0.62, 0.62, 0.38)
	})
}

// IconShield marks NAT and firewall findings.
func IconShield(gtx C, size int, col color.NRGBA) D {
	return iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		p.MoveTo(pt(0.5, 0.10))
		p.LineTo(pt(0.84, 0.24))
		p.LineTo(pt(0.84, 0.52))
		p.QuadTo(pt(0.84, 0.80), pt(0.5, 0.92))
		p.QuadTo(pt(0.16, 0.80), pt(0.16, 0.52))
		p.LineTo(pt(0.16, 0.24))
		p.Close()
	})
}

// IconRouter marks port-mapping results.
func IconRouter(gtx C, size int, col color.NRGBA) D {
	d := iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		rect(p, pt, 0.10, 0.54, 0.80, 0.30)
		line(p, pt, 0.32, 0.54, 0.32, 0.34)
		line(p, pt, 0.32, 0.34, 0.62, 0.20)
		line(p, pt, 0.68, 0.54, 0.68, 0.30)
	})
	dot(gtx, size, col, 0.24, 0.69, 0.05)
	dot(gtx, size, col, 0.40, 0.69, 0.05)
	return d
}

// IconRoute marks the local-interface section.
func IconRoute(gtx C, size int, col color.NRGBA) D {
	d := iconCanvas(gtx, size, col, defaultStroke, func(p *clip.Path, pt func(x, y float32) f32.Point) {
		p.MoveTo(pt(0.22, 0.78))
		p.QuadTo(pt(0.22, 0.50), pt(0.50, 0.50))
		p.QuadTo(pt(0.78, 0.50), pt(0.78, 0.22))
	})
	dot(gtx, size, col, 0.22, 0.80, 0.11)
	dot(gtx, size, col, 0.78, 0.20, 0.11)
	return d
}
