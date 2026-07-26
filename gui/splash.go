package gui

import (
	"image"
	"math"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"
)

// splashView is the loading screen. It covers the window until the service is
// up, which is also the window during which the CJK font is parsed and
// tailscale negotiates its first connection — both slow enough that showing a
// bare grey rectangle would read as a hang.
type splashView struct {
	retry widget.Clickable
	// list keeps the panel reachable on short windows. Without it the retry
	// button — the one control on this screen — falls off the bottom edge once
	// the checklist and an error message are both showing.
	list widget.List
}

func newSplashView() *splashView {
	s := &splashView{}
	s.list.Axis = layout.Vertical
	return s
}

// stepTitles maps supervisor step keys onto localised labels.
func stepTitle(th *Theme, key string) string {
	switch key {
	case core.StepKeyConfig:
		return th.T(KStepConfig)
	case core.StepKeyTsnet:
		return th.T(KStepTsnet)
	case core.StepKeyRules:
		return th.T(KStepRules)
	case core.StepKeyServices:
		return th.T(KStepDiscovery)
	case core.StepKeyMonitors:
		return th.T(KStepMonitors)
	case core.StepKeyReady:
		return th.T(KStepReady)
	default:
		return key
	}
}

func (s *splashView) Layout(a *App, gtx C, st core.State) D {
	th := a.th
	paint.Fill(gtx.Ops, th.P.Bg)

	if s.retry.Clicked(gtx) && a.opt.Supervisor != nil {
		a.opt.Supervisor.Restart()
	}

	// The docked log sheet sits along the bottom edge, so the panel is centred
	// in whatever is left above it. Reserving the space rather than stacking
	// the two is the whole point: a screenshot taken mid-load has to show both
	// the checklist and the log.
	reserve := dockedReserve(gtx)
	if maxReserve := gtx.Constraints.Max.Y / 2; reserve > maxReserve {
		reserve = maxReserve
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx C) D {
			// A single-element list: centred when it fits, scrollable when the
			// window is too short for the checklist plus an error message.
			return material.List(th.Theme, &s.list).Layout(gtx, 1, func(gtx C, _ int) D {
				return layout.Center.Layout(gtx, func(gtx C) D {
					gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Dp(460))
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return layout.Inset{Top: SpaceLG, Bottom: SpaceLG}.Layout(gtx, func(gtx C) D {
						return s.panel(a, gtx, st)
					})
				})
			})
		}),
		layout.Rigid(func(gtx C) D { return D{Size: image.Pt(0, reserve)} }),
	)
}

func (s *splashView) panel(a *App, gtx C, st core.State) D {
	th := a.th
	return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return s.pulse(a, gtx, st)
		}),
		VGap(SpaceMD),
		layout.Rigid(func(gtx C) D {
			l := th.Text(SizeDisplay, th.P.TextPri, "tslink")
			l.Font.Weight = font.Bold
			l.Alignment = text.Middle
			return l.Layout(gtx)
		}),
		layout.Rigid(func(gtx C) D {
			l := th.Caption(th.T(KAppSubtitle))
			l.Alignment = text.Middle
			return l.Layout(gtx)
		}),
		VGap(SpaceLG),
		layout.Rigid(func(gtx C) D {
			return th.ProgressBar(gtx, st.Progress(), th.P.Accent)
		}),
		VGap(SpaceLG),
		layout.Rigid(func(gtx C) D {
			return s.checklist(a, gtx, st)
		}),
		layout.Rigid(func(gtx C) D {
			return s.footer(a, gtx, st)
		}),
	)
}

// pulse draws concentric rings radiating from a solid core. Three rings offset
// in phase read as continuous motion without a spinning element, which suits a
// "connecting to a network" wait better than a rotating arc.
func (s *splashView) pulse(a *App, gtx C, st core.State) D {
	th := a.th
	size := gtx.Dp(64)
	center := f32.Pt(float32(size)/2, float32(size)/2)

	col := th.P.Accent
	switch st.Phase {
	case core.PhaseError:
		col = th.P.Fail
	case core.PhaseRetrying:
		col = th.P.Warn
	}

	const period = 2400 * time.Millisecond
	base := float32(gtx.Dp(14))
	grow := float32(size)/2 - base

	if st.Phase != core.PhaseError {
		phase := float64(gtx.Now.UnixNano()%int64(period)) / float64(period)
		for i := 0; i < 3; i++ {
			p := math.Mod(phase+float64(i)/3, 1)
			r := base + grow*float32(p)
			// Ease the fade so rings vanish before they hit the edge.
			alpha := float32(1-p) * 0.55
			drawArc(gtx, center, r, float32(gtx.Dp(1.5)), 0, 2*math.Pi, WithAlpha(col, alpha))
		}
		// A 2.4s cycle does not need 25fps, and this is the one animation that
		// can legitimately run for minutes while tailscale negotiates.
		animateSlow(gtx)
	}

	// Solid core.
	d := gtx.Dp(22)
	off := op.Offset(image.Pt((size-d)/2, (size-d)/2)).Push(gtx.Ops)
	Circle(gtx, d, col)
	off.Pop()

	return D{Size: image.Pt(size, size)}
}

func (s *splashView) checklist(a *App, gtx C, st core.State) D {
	th := a.th
	children := make([]layout.FlexChild, 0, len(st.Steps))
	for _, step := range st.Steps {
		children = append(children, layout.Rigid(func(gtx C) D {
			return s.stepRow(a, gtx, step)
		}))
	}
	card := th.Card()
	card.Pad = SpaceMD
	card.Bg = &th.P.BgElevated
	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (s *splashView) stepRow(a *App, gtx C, step core.BootStep) D {
	th := a.th
	var (
		fg    = th.P.TextDim
		badge layout.Widget
	)
	switch step.State {
	case core.StepRunning:
		fg = th.P.TextPri
		badge = func(gtx C) D { return th.Spinner(gtx, gtx.Dp(14), th.P.Accent) }
	case core.StepDone:
		fg = th.P.TextSec
		badge = func(gtx C) D { return IconCheck(gtx, gtx.Dp(14), th.P.OK) }
	case core.StepFailed:
		fg = th.P.Fail
		badge = func(gtx C) D { return IconWarn(gtx, gtx.Dp(14), th.P.Fail) }
	case core.StepSkipped:
		badge = func(gtx C) D { return Circle(gtx, gtx.Dp(6), th.P.TextDim) }
	default:
		badge = func(gtx C) D {
			// An empty ring reads as "not started" without adding a colour.
			drawArc(gtx, f32.Pt(7, 7), 5, 1, 0, 2*math.Pi, WithAlpha(th.P.TextDim, 0.5))
			return D{Size: image.Pt(gtx.Dp(14), gtx.Dp(14))}
		}
	}

	return layout.Inset{Top: 5, Bottom: 5}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Dp(18)
				return layout.W.Layout(gtx, badge)
			}),
			HGap(SpaceSM),
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(OneLine(th.Text(SizeBody, fg, stepTitle(th, step.Key))).Layout),
					layout.Rigid(func(gtx C) D {
						if step.Err == "" {
							return D{}
						}
						return OneLine(th.Text(SizeCaption, th.P.Fail, step.Err)).Layout(gtx)
					}),
				)
			}),
			layout.Rigid(func(gtx C) D {
				if step.State != core.StepDone || step.Elapsed() < 100*time.Millisecond {
					return D{}
				}
				return th.MonoLabel(SizeCaption, th.P.TextDim,
					FormatLatency(step.Elapsed())).Layout(gtx)
			}),
		)
	})
}

func (s *splashView) footer(a *App, gtx C, st core.State) D {
	th := a.th
	return layout.Inset{Top: SpaceLG}.Layout(gtx, func(gtx C) D {
		switch st.Phase {
		case core.PhaseError:
			// Error text and the retry button sit side by side: stacking them
			// pushes the only control on this screen below the fold on a short
			// window.
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx C) D {
					l := th.Text(SizeCaption, th.P.Fail, st.Err)
					l.MaxLines = 4
					return l.Layout(gtx)
				}),
				HGap(SpaceMD),
				layout.Rigid(func(gtx C) D {
					gtx.Constraints.Min.X = 0
					return th.Button(gtx, &s.retry, ButtonStyle{
						Kind: ButtonPrimary,
						Text: th.T(KRetry),
						Icon: IconRefresh,
					})
				}),
			)

		case core.PhaseRetrying:
			msg := th.T(KSplashRetry)
			if st.Err != "" {
				msg = st.Err
			}
			l := th.Text(SizeCaption, th.P.Warn, msg)
			l.Alignment = text.Middle
			l.MaxLines = 3
			return l.Layout(gtx)

		default:
			l := th.Caption(th.T(KSplashHint))
			l.Alignment = text.Middle
			return l.Layout(gtx)
		}
	})
}

// ---------------------------------------------------------------------------
// Shared: a translucent panel backdrop
// ---------------------------------------------------------------------------

// glassPanel fills the current bounds with a translucent surface plus border.
// It is what makes the log overlay readable over whatever is behind it while
// still showing that something is behind it.
func glassPanel(t *Theme, gtx C, size image.Point, radius float32) {
	r := int(radius)
	bg := t.P.BgElevated
	bg.A = 0xE0
	paint.FillShape(gtx.Ops, bg, clip.UniformRRect(image.Rectangle{Max: size}, r).Op(gtx.Ops))
	spec := clip.UniformRRect(image.Rectangle{Max: size}, r).Path(gtx.Ops)
	paint.FillShape(gtx.Ops, WithAlpha(t.P.BorderHi, 0.8),
		clip.Stroke{Path: spec, Width: 1}.Op())
}
