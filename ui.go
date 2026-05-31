package main

import (
	"fmt"
	"image"
	"image/color"
	"tslink/core"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

type uiState struct {
	initState int
	nodeName  string
	nodeIP    string
	peers     []core.LinkPeerConnectivityStatus
	history   []float64
	ready     bool
	list      layout.List
	logList   layout.List
	err       string
	logs      []string
}

func run(window *app.Window) error {
	theme := material.NewTheme()
	var ops op.Ops
	state := &uiState{
		nodeName: "unknown",
	}
	state.logList.ScrollToEnd = true
	state.logList.Axis = layout.Vertical
	state.list.Axis = layout.Vertical

	go func() {
		for e := range core.Events {
			switch event := e.(type) {
			case *core.LinkInitEvent:
				state.initState = event.State
				if event.State == core.LinkInitReady {
					state.ready = true
				}
			case *core.LogEvent:
				state.logs = append(state.logs, event.Message)
				if len(state.logs) > 100 {
					state.logs = state.logs[1:]
				}
			case *core.LinkErrorEvent:
				state.err = event.Error
			case *core.HostnameAssignedEvent:
				state.nodeName = event.Hostname
				state.nodeIP = event.IP
			case *core.LinkPeerConnectivityEvent:
				state.peers = event.PingResult
				var totalLat float64
				var count int
				for _, p := range event.PingResult {
					if p.Result != nil {
						totalLat += p.Result.LatencySeconds
						count++
					}
				}
				if count > 0 {
					state.history = append(state.history, totalLat/float64(count))
					if len(state.history) > 50 {
						state.history = state.history[1:]
					}
				}
			}
			window.Invalidate()
		}
	}()

	for {
		switch e := window.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			if !state.ready {
				drawLoading(gtx, theme, state)
			} else {
				drawMain(gtx, theme, state)
			}
			e.Frame(gtx.Ops)
		}
	}
}

func drawLoading(gtx layout.Context, theme *material.Theme, state *uiState) layout.Dimensions {
	return layout.Stack{Alignment: layout.Center}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			return layout.NW.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return state.logList.Layout(gtx, len(state.logs), func(gtx layout.Context, i int) layout.Dimensions {
						l := material.Caption(theme, state.logs[i])
						l.Color = color.NRGBA{R: 150, G: 150, B: 150, A: 80}
						return l.Layout(gtx)
					})
				})
			})
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if state.err != "" {
						return layout.Dimensions{}
					}
					return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return material.Loader(theme).Layout(gtx)
					})
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					txt := "Initializing..."
					textColor := theme.Palette.Fg
					if state.err != "" {
						txt = state.err
						textColor = color.NRGBA{R: 200, G: 0, B: 0, A: 255}
					} else {
						switch state.initState {
						case core.LinkInitFetchConfig:
							txt = "Fetching configuration..."
						case core.LinkInitConnectingTailscale:
							txt = "Connecting to Tailscale..."
						case core.LinkInitControlPlaneConnected:
							txt = "Control plane connected..."
						case core.LinkInitProgramSetup:
							txt = "Setting up program..."
						}
					}
					l := material.Body1(theme, txt)
					l.Color = textColor
					l.Alignment = text.Middle
					return l.Layout(gtx)
				}),
			)
		}),
	)
}

func drawMain(gtx layout.Context, theme *material.Theme, state *uiState) layout.Dimensions {
	return layout.UniformInset(unit.Dp(24)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			// Header Row
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
					// Left Column: Name, IP, Status
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								h3 := material.H3(theme, state.nodeName)
								return h3.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								h4 := material.H4(theme, state.nodeIP)
								h4.Color = color.NRGBA{R: 150, G: 150, B: 150, A: 255}
								return h4.Layout(gtx)
							}),
							layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return material.Body1(theme, "Status: OK").Layout(gtx)
									}),
									layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return drawStatusCircle(gtx, color.NRGBA{R: 0, G: 200, B: 0, A: 255})
									}),
								)
							}),
						)
					}),
					// Spacer (layout.SpaceBetween handles this)
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Dimensions{}
					}),
					// Right Column: Latency Chart
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Dp(unit.Dp(300))
						return drawChart(gtx, state.history)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(24)}.Layout),
			// Divider
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return drawDivider(gtx, theme, "Peers")
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			// Peer List
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return state.list.Layout(gtx, len(state.peers), func(gtx layout.Context, i int) layout.Dimensions {
					p := state.peers[i]
					lat := "N/A"
					conn := "unknown"
					if p.Result != nil {
						lat = fmt.Sprintf("%.2fms", p.Result.LatencySeconds*1000)
						if p.Result.DERPRegionCode == "" {
							conn = "direct"
						} else {
							conn = fmt.Sprintf("derp(%s)", p.Result.DERPRegionCode)
						}
					}
					return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceBetween}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return material.Body1(theme, p.Target.String()).Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								txt := fmt.Sprintf("%s (%s)", lat, conn)
								l := material.Body1(theme, txt)
								l.Alignment = text.End
								return l.Layout(gtx)
							}),
						)
					})
				})
			}),
		)
	})
}

func drawStatusCircle(gtx layout.Context, col color.NRGBA) layout.Dimensions {
	size := gtx.Dp(unit.Dp(12))
	d := image.Pt(size, size)
	defer clip.Ellipse{Max: d}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: col}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	return layout.Dimensions{Size: d}
}

func drawDivider(gtx layout.Context, theme *material.Theme, title string) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.Overline(theme, title).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			rect := image.Rect(0, 0, gtx.Constraints.Max.X, gtx.Dp(unit.Dp(1)))
			paint.FillShape(gtx.Ops, color.NRGBA{R: 200, G: 200, B: 200, A: 50}, clip.Rect(rect).Op())
			return layout.Dimensions{Size: rect.Size()}
		}),
	)
}

func drawChart(gtx layout.Context, history []float64) layout.Dimensions {
	width := gtx.Constraints.Min.X
	if width == 0 {
		width = gtx.Constraints.Max.X
	}
	size := image.Pt(width, 100)

	// Draw Axes
	axisColor := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
	// X axis
	xAxis := image.Rect(0, size.Y-1, size.X, size.Y)
	paint.FillShape(gtx.Ops, axisColor, clip.Rect(xAxis).Op())
	// Y axis
	yAxis := image.Rect(0, 0, 1, size.Y)
	paint.FillShape(gtx.Ops, axisColor, clip.Rect(yAxis).Op())

	if len(history) < 2 {
		return layout.Dimensions{Size: size}
	}

	var maxLat float64
	for _, h := range history {
		if h > maxLat {
			maxLat = h
		}
	}
	if maxLat == 0 {
		maxLat = 1
	}

	var path clip.Path
	path.Begin(gtx.Ops)

	w := float32(size.X)
	h := float32(size.Y)

	xStep := w / float32(len(history)-1)

	for i, lat := range history {
		x := float32(i) * xStep
		y := h - (float32(lat/maxLat) * h * 0.8)

		if i == 0 {
			path.MoveTo(f32.Pt(x, y))
		} else {
			path.LineTo(f32.Pt(x, y))
		}
	}

	paint.FillShape(gtx.Ops, color.NRGBA{R: 0, G: 200, B: 0, A: 255}, clip.Stroke{
		Path:  path.End(),
		Width: float32(gtx.Dp(unit.Dp(2))),
	}.Op())

	return layout.Dimensions{Size: size}
}
