package gui

import (
	"image"
	"log/slog"
	"math"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"

	"gioui.org/op/paint"
)

// Window geometry. The splash is sized to just its progress bar and checklist —
// it has nothing else to show, and a loading screen floating in a 1120x740
// window reads as a broken main window rather than as progress. App.layout
// grows the window to the shell dimensions once the service is ready.
const (
	splashWindowW unit.Dp = 460
	splashWindowH unit.Dp = 450
	splashMinW    unit.Dp = 380
	splashMinH    unit.Dp = 380

	shellWindowW unit.Dp = 1120
	shellWindowH unit.Dp = 740
	shellMinW    unit.Dp = 880
	shellMinH    unit.Dp = 560
)

// stuckAfter is how long a single boot step may run before the splash offers
// the log export. Tailscale's first connection legitimately takes several
// seconds, so this has to be long enough not to cry wolf, but short enough that
// someone staring at a hung step is told what to do about it.
const stuckAfter = 20 * time.Second

// splashView is the loading screen. It covers the window until the service is
// up, which is also the window during which the CJK font is parsed and
// tailscale negotiates its first connection — both slow enough that showing a
// bare grey rectangle would read as a hang.
type splashView struct {
	retry  widget.Clickable
	export widget.Clickable
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

// stalled reports whether a step has been running long enough to look stuck.
func stalled(st core.State) bool {
	for _, step := range st.Steps {
		if step.State == core.StepRunning && step.Elapsed() >= stuckAfter {
			return true
		}
	}
	return false
}

func (s *splashView) Layout(a *App, gtx C, st core.State) D {
	th := a.th
	paint.Fill(gtx.Ops, th.P.Bg)

	if s.retry.Clicked(gtx) && a.opt.Supervisor != nil {
		a.opt.Supervisor.Restart()
	}
	if s.export.Clicked(gtx) {
		s.exportLogs(a)
	}

	// A single-element list: centred when it fits, scrollable when the window is
	// too short for the checklist plus an error message.
	return material.List(th.Theme, &s.list).Layout(gtx, 1, func(gtx C, _ int) D {
		return layout.Center.Layout(gtx, func(gtx C) D {
			gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Dp(400))
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{
				Top: SpaceLG, Bottom: SpaceLG, Left: SpaceMD, Right: SpaceMD,
			}.Layout(gtx, func(gtx C) D {
				return s.panel(a, gtx, st)
			})
		})
	})
}

// exportLogs writes the current buffer to a file and reports where it went.
// This is the splash's replacement for the live log tail: someone looking at a
// stuck boot needs the log in a file they can attach, not on screen.
func (s *splashView) exportLogs(a *App) {
	if a.opt.Logs == nil {
		return
	}
	content := a.opt.Logs.ExportText(core.ExportOptions{
		Header: a.diagnosticHeader(),
		// Debug and up: a stuck boot is exactly when the quiet records matter.
		Query: core.LogQuery{MinLevel: slog.LevelDebug},
	})
	path, err := saveLogFile(content)
	if err != nil {
		a.notify(a.th.T(KError)+": "+err.Error(), LevelFail)
		return
	}
	a.notify(path, LevelOK)
	a.reveal(path)
}

func (s *splashView) panel(a *App, gtx C, st core.State) D {
	th := a.th
	return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
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
				// A running step shows its timer once it is slow enough to be
				// worth watching; a finished one shows what it cost.
				switch {
				case step.State == core.StepRunning && step.Elapsed() >= time.Second:
				case step.State == core.StepDone && step.Elapsed() >= 100*time.Millisecond:
				default:
					return D{}
				}
				col := th.P.TextDim
				if step.State == core.StepRunning && step.Elapsed() >= stuckAfter {
					col = th.P.Warn
				}
				return th.MonoLabel(SizeCaption, col, FormatLatency(step.Elapsed())).Layout(gtx)
			}),
		)
	})
}

func (s *splashView) footer(a *App, gtx C, st core.State) D {
	th := a.th
	stuck := stalled(st)

	return layout.Inset{Top: SpaceLG}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				switch st.Phase {
				case core.PhaseError:
					// Error text and the retry button sit side by side: stacking
					// them pushes the only control on this screen below the fold
					// on a short window.
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
					hint, col := th.T(KSplashHint), th.P.TextDim
					if stuck {
						hint, col = th.T(KSplashStuckHint), th.P.Warn
					}
					l := th.Text(SizeCaption, col, hint)
					l.Alignment = text.Middle
					l.MaxLines = 3
					return l.Layout(gtx)
				}
			}),
			VGap(SpaceMD),
			layout.Rigid(func(gtx C) D {
				if a.opt.Logs == nil {
					return D{}
				}
				// Promoted once something looks stuck: that is the moment the
				// log is worth exporting.
				kind := ButtonGhost
				if stuck || st.Phase == core.PhaseError {
					kind = ButtonSubtle
				}
				gtx.Constraints.Min.X = 0
				return th.Button(gtx, &s.export, ButtonStyle{
					Kind: kind,
					Text: th.T(KSplashExportLog),
					Icon: IconSave,
				})
			}),
		)
	})
}
