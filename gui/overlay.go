package gui

import (
	"log/slog"
	"strings"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/unit"
	"gioui.org/widget"

	"tslink/core"
)

// logOverlay is the translucent live-log panel.
//
// It exists for one specific situation: someone is looking at a stuck loading
// screen and takes a screenshot to ask for help. If the logs are on another
// page, that screenshot is useless. Rendering them as a translucent sheet over
// the loading screen means the interesting information is in the picture
// without hiding what the app is doing.
type logOverlay struct {
	visible  bool
	list     layout.List
	copyBtn  widget.Clickable
	closeBtn widget.Clickable
	// docked is set while the splash is up: the panel then spans the window
	// bottom instead of floating in the corner.
	docked bool

	// Cached tail. The overlay redraws at the animation rate because of its
	// live status dot, but the log only changes when a record is appended, so
	// the slice is rebuilt on sequence change rather than every frame.
	cached    []core.LogEntry
	cachedSeq uint64
	cachedLen int
}

// tail returns the newest records, rebuilding only when the buffer advanced.
func (o *logOverlay) tail(buf *core.LogBuffer) []core.LogEntry {
	seq, n := buf.LastSeq(), buf.Len()
	if o.cached != nil && seq == o.cachedSeq && n == o.cachedLen {
		return o.cached
	}
	o.cached = buf.Tail(overlayTailSize)
	o.cachedSeq, o.cachedLen = seq, n
	return o.cached
}

func newLogOverlay() *logOverlay {
	return &logOverlay{
		list: layout.List{Axis: layout.Vertical, ScrollToEnd: true},
	}
}

// overlayTailSize is how many recent records the overlay renders. The full
// history lives on the logs page; this is a live tail, not an archive.
const overlayTailSize = 400

// Docked geometry. The splash reserves exactly this much room at the bottom of
// the window so the checklist is never hidden behind the log sheet — the point
// of the overlay is that both are legible in one screenshot.
const (
	dockedLogHeight    unit.Dp = 176
	dockedHeaderHeight unit.Dp = 28
)

// dockedReserve is the total vertical space the docked overlay occupies,
// including its insets and the margin below it.
func dockedReserve(gtx C) int {
	return gtx.Dp(dockedLogHeight + dockedHeaderHeight + SpaceSM + SpaceMD*2 + SpaceXL)
}

// Layout draws the overlay. duringSplash forces it visible and docked.
func (o *logOverlay) Layout(a *App, gtx C, duringSplash bool) D {
	o.docked = duringSplash
	if !duringSplash && !o.visible {
		return D{}
	}
	if a.opt.Logs == nil {
		return D{}
	}

	entries := o.tail(a.opt.Logs)

	if o.copyBtn.Clicked(gtx) {
		a.copyToClipboard(gtx, a.opt.Logs.ExportText(core.ExportOptions{
			Header: a.diagnosticHeader(),
			Query:  core.LogQuery{MinLevel: slog.LevelDebug},
		}), a.th.T(KCopied))
	}
	if o.closeBtn.Clicked(gtx) {
		o.visible = false
	}

	if duringSplash {
		return layout.S.Layout(gtx, func(gtx C) D {
			return layout.Inset{
				Left: SpaceXL, Right: SpaceXL, Bottom: SpaceXL,
			}.Layout(gtx, func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return o.panel(a, gtx, entries, dockedLogHeight)
			})
		})
	}
	return layout.SE.Layout(gtx, func(gtx C) D {
		return layout.Inset{Right: SpaceXL, Bottom: SpaceXL}.Layout(gtx, func(gtx C) D {
			w := min(gtx.Constraints.Max.X, gtx.Dp(520))
			gtx.Constraints.Max.X = w
			gtx.Constraints.Min.X = w
			return o.panel(a, gtx, entries, unit.Dp(300))
		})
	})
}

func (o *logOverlay) panel(a *App, gtx C, entries []core.LogEntry, height unit.Dp) D {
	th := a.th
	h := gtx.Dp(height)
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			glassPanel(th, gtx, gtx.Constraints.Min, float32(gtx.Dp(RadiusMD)))
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{
				Top: SpaceMD, Bottom: SpaceMD, Left: SpaceLG, Right: SpaceMD,
			}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D { return o.header(a, gtx, len(entries)) }),
					VGap(SpaceSM),
					layout.Rigid(func(gtx C) D {
						gtx.Constraints.Min.Y = h
						gtx.Constraints.Max.Y = h
						return o.body(a, gtx, entries)
					}),
				)
			})
		}),
	)
}

func (o *logOverlay) header(a *App, gtx C, n int) D {
	th := a.th
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return th.StatusDot(gtx, LevelInfo, false)
		}),
		HGap(SpaceSM),
		layout.Flexed(1, func(gtx C) D {
			l := th.Text(SizeCaption, th.P.TextSec, th.T(KSplashLogHint))
			l.Font.Weight = font.Medium
			return OneLine(l).Layout(gtx)
		}),
		layout.Rigid(func(gtx C) D {
			return th.IconButton(gtx, &o.copyBtn, IconCopy, LevelNeutral)
		}),
		layout.Rigid(func(gtx C) D {
			if o.docked {
				return D{}
			}
			return th.IconButton(gtx, &o.closeBtn, IconClose, LevelNeutral)
		}),
	)
}

func (o *logOverlay) body(a *App, gtx C, entries []core.LogEntry) D {
	th := a.th
	if len(entries) == 0 {
		return layout.Center.Layout(gtx, th.Caption(th.T(KLoading)).Layout)
	}
	defer clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops).Pop()
	return o.list.Layout(gtx, len(entries), func(gtx C, i int) D {
		return o.line(th, gtx, entries[i])
	})
}

// line renders one compact log record: time, level, message, and the most
// useful attributes folded into a single trailing run so the column stays
// narrow.
func (o *logOverlay) line(th *Theme, gtx C, e core.LogEntry) D {
	lvlCol := th.P.TextDim
	switch {
	case e.Level >= slog.LevelError:
		lvlCol = th.P.Fail
	case e.Level >= slog.LevelWarn:
		lvlCol = th.P.Warn
	case e.Level >= slog.LevelInfo:
		lvlCol = th.P.Info
	}

	msgCol := th.P.TextSec
	if e.Level >= slog.LevelWarn {
		msgCol = th.P.TextPri
	}

	return layout.Inset{Top: 1, Bottom: 1}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Start}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return th.MonoLabel(SizeCaption, WithAlpha(th.P.TextDim, 0.85),
					e.Time.Format("15:04:05")).Layout(gtx)
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Dp(26)
				return th.MonoLabel(SizeCaption, lvlCol, core.LevelLabel(e.Level)).Layout(gtx)
			}),
			HGap(SpaceSM),
			layout.Flexed(1, func(gtx C) D {
				l := th.MonoLabel(SizeCaption, msgCol, overlayLineText(e))
				l.MaxLines = 2
				return l.Layout(gtx)
			}),
		)
	})
}

// overlayLineText folds a record's attributes onto one line, dropping the
// "from" attribute because the subsystem is already implied by the message.
func overlayLineText(e core.LogEntry) string {
	var b strings.Builder
	b.WriteString(e.Msg)
	for _, a := range e.Attrs {
		if a.Key == "from" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(Truncate(core.Redact(a.Value), 64))
	}
	return b.String()
}

// diagnosticHeader is the metadata block prepended to any exported log bundle,
// so a paste is self-describing without the reporter having to explain their
// setup.
func (a *App) diagnosticHeader() string {
	st := a.state()
	var b strings.Builder
	b.WriteString("# tslink diagnostic bundle\n")
	b.WriteString("# version: " + a.opt.Version + "\n")
	b.WriteString("# os/arch: " + runtimeInfo() + "\n")
	if a.opt.ConfigURL != "" {
		b.WriteString("# config: (url)\n")
	} else if a.opt.ConfigPath != "" {
		b.WriteString("# config: " + a.opt.ConfigPath + "\n")
	}
	b.WriteString("# phase: " + st.Phase.String() + "\n")
	b.WriteString("# restarts: " + itoa(st.Restarts) + "\n")
	if !st.ReadyAt.IsZero() {
		b.WriteString("# uptime: " + FormatDuration(timeSince(st.ReadyAt)) + "\n")
	}
	if st.Peers != nil {
		snap := st.Peers.Snapshot()
		b.WriteString("# tailnet: " + snap.TailnetName + "\n")
		b.WriteString("# peers: " + itoa(len(snap.Peers)) + "\n")
	}
	// reportText takes the diag page's lock; reading a.diag.report directly
	// would race the background diagnostic goroutine.
	if a.diag != nil {
		if txt := a.diag.reportText(); txt != "" {
			b.WriteString("#\n")
			b.WriteString(txt)
		}
	}
	return b.String()
}
