package gui

import (
	"context"
	"image"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/font"
	"gioui.org/io/clipboard"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/widget"

	"tslink/core"
)

// Options configures the GUI.
type Options struct {
	Version    string
	ConfigPath string
	ConfigURL  string
	Supervisor *core.Supervisor
	Logs       *core.LogBuffer
	Logger     *slog.Logger
	// IPInfoToken is passed through to the diagnostics runner.
	IPInfoToken string
	// StartDark selects the initial theme.
	StartDark bool
}

// pageID identifies a top-level view.
type pageID int

const (
	pageOverview pageID = iota
	pagePeers
	pageDiag
	pageLogs
	pageSettings
)

type navEntry struct {
	id    pageID
	label Key
	icon  IconFunc
	click widget.Clickable
}

// App is the whole GUI. It owns the window event loop and holds every page's
// state.
type App struct {
	opt    Options
	logger *slog.Logger
	th     *Theme
	fonts  *FontSet

	win *app.Window

	nav     []navEntry
	current pageID

	overview *overviewPage
	peers    *peersPage
	diag     *diagPage
	logs     *logsPage
	settings *settingsPage

	splash *splashView

	themeBtn widget.Clickable

	// grown records that the window has already been resized from its compact
	// splash dimensions to the full shell. The splash sizes the window to just
	// its progress bar and checklist, so the transition to the shell has to grow
	// it — exactly once, or a user who resized the window would have it snapped
	// back every time the service restarted. Atomic because the resize runs on
	// the watch goroutine, not the UI one.
	grown atomic.Bool

	toastMsg   string
	toastLevel StatusLevel
	toastUntil time.Time

	// fontUpgrade carries CJK faces parsed off the UI goroutine.
	fontUpgrade chan []font.FontFace

	// needsTick is set during layout when the current frame shows something
	// that changes with wall-clock time — relative timestamps, uptime, a
	// running step's elapsed counter. When it is false the periodic refresh is
	// skipped and the window stops repainting altogether.
	//
	// This is not micro-optimisation: a full repaint costs tens of
	// milliseconds under software rendering (Gio stencils every rounded
	// rectangle and icon as a path), so a once-a-second refresh of a screen
	// with nothing time-dependent on it is pure waste.
	needsTick atomic.Bool
}

// New builds the application.
func New(opt Options) *App {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	fonts := LoadFonts()
	th := NewTheme(fonts, opt.StartDark)

	a := &App{
		opt:         opt,
		logger:      logger.With("from", "gui"),
		th:          th,
		fonts:       fonts,
		current:     pageOverview,
		fontUpgrade: make(chan []font.FontFace, 1),
	}
	a.nav = []navEntry{
		{id: pageOverview, label: KNavOverview, icon: IconGrid},
		{id: pagePeers, label: KNavPeers, icon: IconNodes},
		{id: pageDiag, label: KNavDiag, icon: IconPulse},
		{id: pageLogs, label: KNavLogs, icon: IconList},
		{id: pageSettings, label: KNavSettings, icon: IconSliders},
	}
	a.overview = newOverviewPage()
	a.peers = newPeersPage()
	a.diag = newDiagPage(a)
	a.logs = newLogsPage(a)
	a.settings = newSettingsPage(a)
	a.splash = newSplashView()
	return a
}

// Run opens the window and drives the event loop. It returns when the window
// closes.
func (a *App) Run(ctx context.Context) error {
	w := new(app.Window)
	// Opens at splash size; grown to the shell dimensions below once the
	// service is ready.
	w.Option(
		app.Title("tslink"),
		app.Size(splashWindowW, splashWindowH),
		app.MinSize(splashMinW, splashMinH),
	)
	a.win = w

	go a.watch(ctx, w)
	go a.upgradeFonts()
	// Ctrl+C at the terminal cancels ctx. Without this the supervisor tears
	// down but the window survives, dropping the user back to the splash — the
	// GUI is the process, so cancelling it has to close the window too.
	go func() {
		<-ctx.Done()
		w.Perform(system.ActionClose)
	}()

	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			a.applyFontUpgrade()
			a.layout(gtx)
			e.Frame(gtx.Ops)
			// After the frame, so the resize is not applied midway through
			// laying one out against the old constraints. Once grown this is a
			// single atomic load.
			if !a.grown.Load() && a.state().Ready() {
				a.grow(w)
			}
		}
	}
}

// grow resizes the window from its compact splash dimensions to the full shell,
// once, when the service comes up.
//
// It must run on the goroutine that drives the event loop. On Linux the driver
// executes Window.Option's work inline on the calling goroutine rather than
// handing it to a UI thread, so calling this from anywhere else would mutate
// driver state concurrently with event dispatch.
func (a *App) grow(w *app.Window) {
	if a.grown.Swap(true) {
		return
	}
	w.Option(
		app.Size(shellWindowW, shellWindowH),
		app.MinSize(shellMinW, shellMinH),
	)
	w.Invalidate()
}

// upgradeFonts parses the system CJK font off the UI goroutine. The splash
// screen exists partly to cover this: a 20 MB font collection takes long
// enough to parse that doing it inline would stall the first frame.
func (a *App) upgradeFonts() {
	if !a.fonts.HasCJK || a.fonts.CJKPath == "" {
		return
	}
	faces, err := LoadCJKFaces(a.fonts.CJKPath, a.logger)
	if err != nil {
		a.logger.Warn("failed to load cjk font, relying on system fallback",
			"path", a.fonts.CJKPath, "err", err)
		return
	}
	if len(faces) == 0 {
		return
	}
	select {
	case a.fontUpgrade <- faces:
		if a.win != nil {
			a.win.Invalidate()
		}
	default:
	}
}

func (a *App) applyFontUpgrade() {
	select {
	case faces := <-a.fontUpgrade:
		merged := append(append([]font.FontFace(nil), a.fonts.Collection...), faces...)
		a.fonts.Collection = merged
		a.th.Shaper = text.NewShaper(text.WithCollection(merged))
		a.logger.Debug("shaper upgraded with cjk faces", "faces", len(faces))
	default:
	}
}

// watch coalesces change notifications from every data source into window
// invalidations, capped so a burst of log lines cannot drive the render loop.
func (a *App) watch(ctx context.Context, w *app.Window) {
	var chans []<-chan struct{}
	var cancels []func()
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	if a.opt.Supervisor != nil {
		ch, cancel := a.opt.Supervisor.Subscribe()
		chans = append(chans, ch)
		cancels = append(cancels, cancel)
	}
	if a.opt.Logs != nil {
		ch, cancel := a.opt.Logs.Subscribe()
		chans = append(chans, ch)
		cancels = append(cancels, cancel)
	}

	// A ticker keeps relative timestamps ("3m ago") and the live latency
	// column honest even when nothing else changed.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	dirty := false
	throttle := time.NewTicker(70 * time.Millisecond)
	defer throttle.Stop()

	agg := make(chan struct{}, 1)
	for _, ch := range chans {
		go func(ch <-chan struct{}) {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
					select {
					case agg <- struct{}{}:
					default:
					}
				}
			}
		}(ch)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-agg:
			dirty = true
		case <-tick.C:
			if a.needsTick.Load() {
				dirty = true
			}
		case <-throttle.C:
			if dirty {
				dirty = false
				w.Invalidate()
			}
		}
	}
}

// state returns the current supervisor snapshot, or a zero value.
func (a *App) state() core.State {
	if a.opt.Supervisor == nil {
		return core.State{}
	}
	return a.opt.Supervisor.Snapshot()
}

// ---------------------------------------------------------------------------
// Clipboard + toast
// ---------------------------------------------------------------------------

// copyToClipboard puts s on the system clipboard and shows a confirmation.
func (a *App) copyToClipboard(gtx C, s string, msg string) {
	gtx.Execute(clipboard.WriteCmd{
		Type: "application/text",
		Data: io.NopCloser(strings.NewReader(s)),
	})
	if msg == "" {
		msg = a.th.T(KCopied)
	}
	a.notify(msg, LevelOK)
}

// reveal shows path in the platform file manager, off the UI goroutine so a
// slow or missing file manager cannot stall a frame. Failure is logged rather
// than surfaced: the file is already written and its path is already on screen,
// so there is nothing for the user to act on.
func (a *App) reveal(path string) {
	go func() {
		if err := RevealInFileManager(path, a.logger); err != nil {
			a.logger.Warn("could not open the file manager", "path", path, "err", err)
		}
	}()
}

// notify shows a transient message at the bottom of the window.
func (a *App) notify(msg string, level StatusLevel) {
	a.toastMsg = msg
	a.toastLevel = level
	a.toastUntil = time.Now().Add(3200 * time.Millisecond)
	if a.win != nil {
		a.win.Invalidate()
	}
}

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

func (a *App) layout(gtx C) D {
	th := a.th
	paint.Fill(gtx.Ops, th.P.Bg)

	st := a.state()

	// A terminal error screen has nothing that ages; everything else does
	// (uptime, "last seen", a running step's timer).
	a.needsTick.Store(st.Phase != core.PhaseError && st.Phase != core.PhaseStopped)

	// Handle nav clicks before drawing so the click lands on this frame.
	for i := range a.nav {
		if a.nav[i].click.Clicked(gtx) {
			a.current = a.nav[i].id
		}
	}
	if a.themeBtn.Clicked(gtx) {
		th.SetDark(!th.Dark)
	}

	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min = gtx.Constraints.Max
			if !st.Ready() {
				// The splash owns the whole window until the service is up.
				return a.splash.Layout(a, gtx, st)
			}
			return a.shell(gtx, st)
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min = gtx.Constraints.Max
			return a.layoutToast(gtx)
		}),
	)
}

// shell draws the sidebar plus the active page.
func (a *App) shell(gtx C, st core.State) D {
	compact := gtx.Constraints.Max.X < gtx.Dp(1000)
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return a.sidebar(gtx, compact)
		}),
		layout.Flexed(1, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D { return a.header(gtx, st) }),
				layout.Flexed(1, func(gtx C) D {
					return layout.Inset{
						Left: SpaceXL, Right: SpaceXL, Top: SpaceLG, Bottom: SpaceLG,
					}.Layout(gtx, func(gtx C) D {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return a.page(gtx, st)
					})
				}),
			)
		}),
	)
}

func (a *App) page(gtx C, st core.State) D {
	switch a.current {
	case pagePeers:
		return a.peers.Layout(a, gtx, st)
	case pageDiag:
		return a.diag.Layout(a, gtx, st)
	case pageLogs:
		return a.logs.Layout(a, gtx, st)
	case pageSettings:
		return a.settings.Layout(a, gtx, st)
	default:
		return a.overview.Layout(a, gtx, st)
	}
}

func (a *App) sidebar(gtx C, compact bool) D {
	th := a.th
	w := gtx.Dp(212)
	if compact {
		w = gtx.Dp(64)
	}
	gtx.Constraints.Min.X = w
	gtx.Constraints.Max.X = w

	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			size := image.Pt(w, gtx.Constraints.Max.Y)
			paint.FillShape(gtx.Ops, th.P.BgElevated, clip.Rect{Max: size}.Op())
			// Hairline separating rail from content.
			paint.FillShape(gtx.Ops, th.P.Border, clip.Rect{
				Min: image.Pt(size.X-1, 0), Max: size,
			}.Op())
			return D{Size: size}
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min.X = w
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D { return a.brand(gtx, compact) }),
				layout.Rigid(func(gtx C) D {
					children := make([]layout.FlexChild, 0, len(a.nav))
					for i := range a.nav {
						children = append(children, layout.Rigid(func(gtx C) D {
							return a.navItem(gtx, &a.nav[i], compact)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				}),
			)
		}),
	)
}

func (a *App) brand(gtx C, compact bool) D {
	th := a.th
	return layout.Inset{
		Top: SpaceXL, Bottom: SpaceLG, Left: SpaceLG, Right: SpaceLG,
	}.Layout(gtx, func(gtx C) D {
		if compact {
			return layout.Center.Layout(gtx, func(gtx C) D {
				return IconBroadcast(gtx, gtx.Dp(22), th.P.Accent)
			})
		}
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return IconBroadcast(gtx, gtx.Dp(20), th.P.Accent)
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						l := th.Text(SizeSubtitle, th.P.TextPri, "tslink")
						l.Font.Weight = font.Bold
						return l.Layout(gtx)
					}),
					layout.Rigid(OneLine(th.Caption(th.T(KAppSubtitle))).Layout),
				)
			}),
		)
	})
}

func (a *App) navItem(gtx C, n *navEntry, compact bool) D {
	th := a.th
	selected := a.current == n.id
	fg := th.P.TextSec
	if selected {
		fg = th.P.TextPri
	} else if n.click.Hovered() {
		fg = th.P.TextPri
	}

	return n.click.Layout(gtx, func(gtx C) D {
		return layout.Inset{Left: SpaceSM, Right: SpaceSM, Top: 2, Bottom: 2}.Layout(gtx, func(gtx C) D {
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx C) D {
					size := gtx.Constraints.Min
					switch {
					case selected:
						FillRRect(gtx, size, RadiusSM, WithAlpha(th.P.Accent, 0.16))
						paint.FillShape(gtx.Ops, th.P.Accent, clip.UniformRRect(
							image.Rect(0, size.Y/2-gtx.Dp(8), gtx.Dp(3), size.Y/2+gtx.Dp(8)),
							gtx.Dp(2)).Op(gtx.Ops))
					case n.click.Hovered():
						FillRRect(gtx, size, RadiusSM, th.P.SurfaceHi)
					}
					return D{Size: size}
				}),
				layout.Stacked(func(gtx C) D {
					pad := layout.Inset{Top: 9, Bottom: 9, Left: SpaceMD, Right: SpaceMD}
					if compact {
						pad = layout.Inset{Top: 10, Bottom: 10}
					}
					return pad.Layout(gtx, func(gtx C) D {
						if compact {
							return layout.Center.Layout(gtx, func(gtx C) D {
								return n.icon(gtx, gtx.Dp(19), fg)
							})
						}
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx C) D {
								return n.icon(gtx, gtx.Dp(17), fg)
							}),
							HGap(SpaceMD),
							layout.Rigid(func(gtx C) D {
								l := th.Text(SizeBody, fg, th.T(n.label))
								if selected {
									l.Font.Weight = font.Medium
								}
								return l.Layout(gtx)
							}),
						)
					})
				}),
			)
		})
	})
}

func (a *App) header(gtx C, st core.State) D {
	th := a.th
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			size := gtx.Constraints.Min
			paint.FillShape(gtx.Ops, th.P.Border, clip.Rect{
				Min: image.Pt(0, size.Y-1), Max: size,
			}.Op())
			return D{Size: size}
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{
				Left: SpaceXL, Right: SpaceXL, Top: SpaceLG, Bottom: SpaceMD,
			}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx C) D {
						return th.Title(a.pageTitle()).Layout(gtx)
					}),
					layout.Rigid(func(gtx C) D { return a.statusPill(gtx, st) }),
					HGap(SpaceSM),
					layout.Rigid(func(gtx C) D {
						return th.IconButton(gtx, &a.themeBtn, IconGlobe, LevelNeutral)
					}),
				)
			})
		}),
	)
}

func (a *App) pageTitle() string {
	th := a.th
	for _, n := range a.nav {
		if n.id == a.current {
			return th.T(n.label)
		}
	}
	return "tslink"
}

func (a *App) statusPill(gtx C, st core.State) D {
	th := a.th
	var (
		label string
		level StatusLevel
		pulse bool
	)
	switch st.Phase {
	case core.PhaseReady:
		// Steady state: no animation. See [Theme.StatusDot].
		label, level = th.T(KStateRunning), LevelOK
	case core.PhaseStarting:
		label, level = th.T(KStateConnecting), LevelInfo
		pulse = true
	case core.PhaseRetrying:
		label, level = th.T(KStateRetrying), LevelWarn
		pulse = true
	case core.PhaseError:
		label, level = th.T(KStateError), LevelFail
	case core.PhaseStopped:
		label, level = th.T(KStateStopped), LevelNeutral
	default:
		label, level = th.T(KStateStarting), LevelNeutral
	}

	fg := th.StatusColor(level)
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			FillRRect(gtx, gtx.Constraints.Min, RadiusPill, WithAlpha(fg, 0.13))
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			return layout.Inset{Top: 5, Bottom: 5, Left: SpaceMD, Right: SpaceMD}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return th.StatusDot(gtx, level, pulse)
					}),
					HGap(SpaceSM),
					layout.Rigid(th.Text(SizeCaption, fg, label).Layout),
				)
			})
		}),
	)
}

func (a *App) layoutToast(gtx C) D {
	if a.toastMsg == "" || time.Now().After(a.toastUntil) {
		return D{}
	}
	th := a.th
	// Keep repainting until the toast expires.
	gtx.Execute(op.InvalidateCmd{At: a.toastUntil})

	return layout.S.Layout(gtx, func(gtx C) D {
		return layout.Inset{Bottom: Space2XL}.Layout(gtx, func(gtx C) D {
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx C) D {
					FillRRect(gtx, gtx.Constraints.Min, RadiusSM, th.P.SurfaceHi)
					StrokeRRect(gtx, gtx.Constraints.Min, RadiusSM, 1, th.P.Border)
					return D{Size: gtx.Constraints.Min}
				}),
				layout.Stacked(func(gtx C) D {
					return layout.Inset{
						Top: SpaceSM, Bottom: SpaceSM, Left: SpaceLG, Right: SpaceLG,
					}.Layout(gtx, func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx C) D {
								return th.StatusDot(gtx, a.toastLevel, false)
							}),
							HGap(SpaceSM),
							layout.Rigid(th.Text(SizeBody, th.P.TextPri, a.toastMsg).Layout),
						)
					})
				}),
			)
		})
	})
}

// ---------------------------------------------------------------------------
// Section heading used by pages
// ---------------------------------------------------------------------------

// sectionTitle renders a page-level heading with an optional trailing widget.
func (a *App) sectionTitle(gtx C, title, subtitle string, trailing layout.Widget) D {
	th := a.th
	return layout.Inset{Bottom: SpaceMD}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						l := th.Text(SizeSubtitle, th.P.TextPri, title)
						l.Font.Weight = font.SemiBold
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx C) D {
						if subtitle == "" {
							return D{}
						}
						return th.Caption(subtitle).Layout(gtx)
					}),
				)
			}),
			layout.Rigid(func(gtx C) D {
				if trailing == nil {
					return D{}
				}
				return trailing(gtx)
			}),
		)
	})
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
