package gui

import (
	"image"
	"testing"
	"time"

	"gioui.org/layout"

	"tslink/netdiag"
)

// TestChartDomainFillsWithSparseData is the regression for the blank-chart bug:
// a handful of samples used to occupy the left 3% of a fixed 20-minute axis.
// The domain must track the data, not the clock.
func TestChartDomainFillsWithSparseData(t *testing.T) {
	now := time.Now()
	// 30 seconds of uptime at the 10s ping interval.
	pts := []ChartPoint{
		{At: now.Add(-20 * time.Second), Value: 10, OK: true},
		{At: now.Add(-10 * time.Second), Value: 12, OK: true},
		{At: now, Value: 11, OK: true},
	}
	series := []ChartSeries{{Points: pts}}
	st := ChartStyle{MaxWindow: 3 * time.Minute, Now: now}

	tMin, tMax := domain(series, st)
	if got := tMax.Sub(tMin); got != 20*time.Second {
		t.Fatalf("span = %v, want the data's own 20s extent", got)
	}
	if !tMin.Equal(pts[0].At) || !tMax.Equal(pts[2].At) {
		t.Errorf("domain = [%v, %v], want the first and last sample", tMin, tMax)
	}
}

func TestChartDomainClampsToWindow(t *testing.T) {
	now := time.Now()
	series := []ChartSeries{{Points: []ChartPoint{
		{At: now.Add(-30 * time.Minute), Value: 10, OK: true},
		{At: now, Value: 11, OK: true},
	}}}
	st := ChartStyle{MaxWindow: 3 * time.Minute, Now: now}

	tMin, tMax := domain(series, st)
	if got := tMax.Sub(tMin); got != 3*time.Minute {
		t.Fatalf("span = %v, want it clamped to MaxWindow", got)
	}
}

func TestChartDomainEdgeCases(t *testing.T) {
	now := time.Now()
	st := ChartStyle{MaxWindow: 3 * time.Minute, Now: now}

	// No data at all: fall back to the full window so the grid still renders.
	tMin, tMax := domain(nil, st)
	if got := tMax.Sub(tMin); got != 3*time.Minute {
		t.Errorf("empty span = %v, want the full window", got)
	}

	// One sample would otherwise give a zero-width axis and divide by zero.
	one := []ChartSeries{{Points: []ChartPoint{{At: now, Value: 5, OK: true}}}}
	tMin, tMax = domain(one, st)
	if got := tMax.Sub(tMin); got != minPlotSpan {
		t.Errorf("single-point span = %v, want minPlotSpan", got)
	}

	// Hidden series must not widen the axis.
	mixed := []ChartSeries{
		{Hidden: true, Points: []ChartPoint{{At: now.Add(-2 * time.Minute), Value: 1, OK: true}}},
		{Points: []ChartPoint{
			{At: now.Add(-30 * time.Second), Value: 1, OK: true},
			{At: now, Value: 2, OK: true},
		}},
	}
	tMin, tMax = domain(mixed, st)
	if got := tMax.Sub(tMin); got != 30*time.Second {
		t.Errorf("span = %v, want only the visible series to count", got)
	}
}

// TestWrapRowWraps checks that children exceeding the width land on new lines
// instead of being clipped, which is what a plain Flex did.
func TestWrapRowWraps(t *testing.T) {
	const (
		childW = 100
		childH = 20
		rowW   = 250 // fits 2 children per line
		n      = 5
	)
	child := func(gtx C) D { return D{Size: image.Pt(childW, childH)} }
	children := make([]layout.Widget, n)
	for i := range children {
		children[i] = child
	}

	gtx, _ := newTestContext(image.Pt(rowW, 500))
	dims := WrapRow(gtx, 0, children)

	// 5 children, 2 per line => 3 lines.
	if want := 3 * childH; dims.Size.Y != want {
		t.Errorf("height = %d, want %d (3 wrapped lines)", dims.Size.Y, want)
	}
	if dims.Size.X != rowW {
		t.Errorf("width = %d, want the full %d", dims.Size.X, rowW)
	}
}

func TestWrapRowSingleLine(t *testing.T) {
	child := func(gtx C) D { return D{Size: image.Pt(50, 20)} }
	gtx, _ := newTestContext(image.Pt(500, 500))
	dims := WrapRow(gtx, 0, []layout.Widget{child, child, child})
	if dims.Size.Y != 20 {
		t.Errorf("height = %d, want a single 20px line", dims.Size.Y)
	}
}

func TestWrapRowEmpty(t *testing.T) {
	gtx, _ := newTestContext(image.Pt(100, 100))
	if dims := WrapRow(gtx, 0, nil); dims.Size != (image.Point{}) {
		t.Errorf("want zero dims for no children, got %v", dims.Size)
	}
}

// TestUDPProbeLabel covers the naming rules for the UDP table: prefer the
// configured hostname over the resolved address, and keep the two rows of a
// dual-stack server distinguishable.
func TestUDPProbeLabel(t *testing.T) {
	cases := []struct {
		name string
		in   netdiag.UDPProbe
		want string
	}{
		{"resolved v4", netdiag.UDPProbe{Host: "stun.miwifi.com:3478", Target: "111.206.174.2:3478", Name: "小米"},
			"小米 stun.miwifi.com:3478 · IPv4"},
		{"resolved v6", netdiag.UDPProbe{Host: "stun.miwifi.com:3478", Target: "[2408::1]:3478", Name: "小米"},
			"小米 stun.miwifi.com:3478 · IPv6"},
		{"dns failure keeps the hostname", netdiag.UDPProbe{Host: "a.example:3478", Target: "a.example:3478", Name: "X"},
			"X a.example:3478"},
		{"no name", netdiag.UDPProbe{Host: "a.example:3478", Target: "a.example:3478"}, "a.example:3478"},
		{"no host falls back to target", netdiag.UDPProbe{Target: "1.2.3.4:3478"}, "1.2.3.4:3478 · IPv4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := udpProbeLabel(tc.in); got != tc.want {
				t.Errorf("udpProbeLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}
