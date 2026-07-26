package gui

import (
	"context"
	"image"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"
	"tslink/netdiag"
)

type diagPage struct {
	app  *App
	list widget.List

	runBtn  widget.Clickable
	copyBtn widget.Clickable
	// skipGeo is owned here because the run reads it, but it is presented on
	// the settings page — it is a privacy choice about the next run, not a
	// finding about this one.
	skipGeo widget.Bool

	// mu guards everything the background run writes.
	mu       sync.Mutex
	running  bool
	report   *netdiag.Report
	progress map[string]netdiag.Progress
	order    []string
	lastRun  time.Time
	runErr   string
	cancel   context.CancelFunc
}

func newDiagPage(a *App) *diagPage {
	p := &diagPage{
		app:      a,
		progress: make(map[string]netdiag.Progress),
	}
	p.list.Axis = layout.Vertical
	return p
}

func diagLevel(s netdiag.Status) StatusLevel {
	switch s {
	case netdiag.StatusOK:
		return LevelOK
	case netdiag.StatusWarn:
		return LevelWarn
	case netdiag.StatusFail:
		return LevelFail
	default:
		return LevelNeutral
	}
}

// reportText renders the last report for inclusion in a shared bundle.
func (p *diagPage) reportText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.report == nil {
		return ""
	}
	return p.report.Text()
}

// run starts a diagnostic sweep on a background goroutine.
func (p *diagPage) run() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.running = true
	p.cancel = cancel
	p.progress = make(map[string]netdiag.Progress)
	p.order = nil
	p.runErr = ""
	skipGeo := p.skipGeo.Value
	p.mu.Unlock()

	a := p.app
	st := a.state()
	var src netdiag.TailscaleSource
	if st.Server != nil {
		src = core.DefaultTailscaleSource(st.Server, a.logger)
	}

	go func() {
		defer cancel()
		rep := netdiag.Run(ctx, netdiag.Options{
			Logger:      a.logger.With("from", "netdiag"),
			Tailscale:   src,
			IPInfoToken: a.opt.IPInfoToken,
			SkipGeo:     skipGeo,
			OnProgress: func(pr netdiag.Progress) {
				p.mu.Lock()
				if _, seen := p.progress[pr.Key]; !seen {
					p.order = append(p.order, pr.Key)
				}
				p.progress[pr.Key] = pr
				p.mu.Unlock()
				a.invalidate()
			},
		})
		p.mu.Lock()
		p.report = rep
		p.running = false
		p.lastRun = time.Now()
		p.cancel = nil
		p.mu.Unlock()
		a.invalidate()
	}()
}

func (p *diagPage) Layout(a *App, gtx C, st core.State) D {
	th := a.th

	if p.runBtn.Clicked(gtx) {
		p.run()
	}
	if p.copyBtn.Clicked(gtx) {
		if txt := p.reportText(); txt != "" {
			a.copyToClipboard(gtx, txt, th.T(KCopied))
		}
	}

	p.mu.Lock()
	running := p.running
	report := p.report
	lastRun := p.lastRun
	progress := make([]netdiag.Progress, 0, len(p.order))
	for _, k := range p.order {
		progress = append(progress, p.progress[k])
	}
	p.mu.Unlock()

	items := []layout.Widget{
		func(gtx C) D { return p.controlCard(a, gtx, running, report, lastRun, progress) },
	}
	if report != nil {
		items = append(items,
			func(gtx C) D { return p.natCard(a, gtx, report.NAT) },
			func(gtx C) D { return p.udpCard(a, gtx, report.UDP) },
			func(gtx C) D { return p.portMapCard(a, gtx, report.PortMap) },
			func(gtx C) D { return p.overseasCard(a, gtx, report.Overseas) },
			func(gtx C) D { return p.egressCard(a, gtx, report.Egress) },
			func(gtx C) D { return p.ifaceCard(a, gtx, report.Interfaces) },
			func(gtx C) D { return p.tailscaleCard(a, gtx, report.Tailscale) },
		)
	}

	return material.List(th.Theme, &p.list).Layout(gtx, len(items), func(gtx C, i int) D {
		return layout.Inset{Bottom: SpaceMD}.Layout(gtx, items[i])
	})
}

// controlCard is the page's anchor: what the verdict is, when it was measured,
// and how to measure again.
func (p *diagPage) controlCard(a *App, gtx C, running bool, rep *netdiag.Report, lastRun time.Time, progress []netdiag.Progress) D {
	th := a.th
	card := th.Card()
	if rep != nil {
		// The accent tracks the headline's own severity, not Report.Status.
		// Report.Status is the worst of every section, so one router without
		// UPnP painted the whole card amber underneath a sentence saying the
		// network was fine — the accent and the words have to agree.
		accent := th.StatusColor(diagLevel(rep.HeadlineStatus))
		card.Accent = &accent
	}
	// Once a report exists the run's own scaffolding has nothing left to say:
	// the step list has been replaced by the chip rows, and the geolocation
	// toggle only applies to the next run.
	settled := rep != nil && !running

	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx C) D {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx C) D {
								headline := th.T(KDiagNever)
								col := th.P.TextSec
								if running {
									headline = th.T(KDiagRunning) + "…"
									col = th.P.TextPri
								} else if rep != nil {
									headline = rep.Headline
									// The headline's own severity, not the report's: an
									// unrelated failure elsewhere must not paint a merely
									// cautionary sentence in alarm red.
									col = th.StatusColor(diagLevel(rep.HeadlineStatus))
								}
								l := th.Text(SizeSubtitle, col, headline)
								l.Font.Weight = font.SemiBold
								l.MaxLines = 3
								return l.Layout(gtx)
							}),
							layout.Rigid(func(gtx C) D {
								if lastRun.IsZero() {
									return D{}
								}
								// Only when, not how long: the run duration is a
								// property of the probe schedule, not of the network
								// being probed, so it told the user nothing.
								txt := th.T(KDiagLastRun) + " " + RelTime(th, lastRun, time.Now())
								return layout.Inset{Top: 2}.Layout(gtx, th.Caption(txt).Layout)
							}),
						)
					}),
					HGap(SpaceMD),
					layout.Rigid(func(gtx C) D {
						if rep == nil {
							return D{}
						}
						return th.Button(gtx, &p.copyBtn, ButtonStyle{
							Kind: ButtonSubtle,
							Text: th.T(KDiagCopyReport),
							Icon: IconCopy,
						})
					}),
					HGap(SpaceSM),
					layout.Rigid(func(gtx C) D {
						label := th.T(KDiagRun)
						if rep != nil {
							label = th.T(KDiagRerun)
						}
						if running {
							label = th.T(KDiagRunning)
						}
						return th.Button(gtx, &p.runBtn, ButtonStyle{
							Kind:     ButtonPrimary,
							Text:     label,
							Icon:     IconRefresh,
							Disabled: running,
						})
					}),
				)
			}),
			layout.Rigid(func(gtx C) D {
				if rep == nil {
					return D{}
				}
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					labelW := p.summaryLabelWidth(a, gtx)
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							return p.summaryRow(a, gtx, labelW, th.T(KDiagGroupConn), connChips(th, rep))
						}),
						VGap(SpaceSM),
						layout.Rigid(func(gtx C) D {
							return p.summaryRow(a, gtx, labelW, th.T(KDiagGroupEnv), envChips(th, rep))
						}),
					)
				})
			}),
			layout.Rigid(func(gtx C) D {
				if settled || (!running && len(progress) == 0) {
					return D{}
				}
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					return p.progressList(a, gtx, progress, running)
				})
			}),
			// The geolocation toggle lives on the settings page: it is run
			// configuration, not a result, and this card is only about results
			// once one exists.
		)
	})
}

func (p *diagPage) progressList(a *App, gtx C, progress []netdiag.Progress, running bool) D {
	th := a.th
	children := make([]layout.FlexChild, 0, len(progress))
	for _, pr := range progress {
		children = append(children, layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 3, Bottom: 3}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						gtx.Constraints.Min.X = gtx.Dp(18)
						switch {
						case pr.Err != "":
							return IconWarn(gtx, gtx.Dp(13), th.P.Warn)
						case pr.Done:
							return IconCheck(gtx, gtx.Dp(13), th.P.OK)
						default:
							return th.Spinner(gtx, gtx.Dp(13), th.P.Accent)
						}
					}),
					HGap(SpaceSM),
					layout.Flexed(1, func(gtx C) D {
						col := th.P.TextSec
						if !pr.Done {
							col = th.P.TextPri
						}
						return OneLine(th.Text(SizeCaption, col, pr.Title)).Layout(gtx)
					}),
					layout.Rigid(func(gtx C) D {
						if !pr.Done || pr.Elapsed <= 0 {
							return D{}
						}
						return th.MonoLabel(SizeCaption, th.P.TextDim,
							FormatLatency(pr.Elapsed)).Layout(gtx)
					}),
				)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// ---------------------------------------------------------------------------
// Summary rows
// ---------------------------------------------------------------------------

// summaryChip is one reading in the grouped rows at the top of the page.
//
// Level and Mark are separate because colour and glyph answer different
// questions. A missing IPv6 is a real "no" — it earns a cross — but on most
// Chinese home lines it is also entirely normal, so painting it red would train
// the reader to ignore the colour on the rows that do matter.
type summaryChip struct {
	Label string
	Value string
	Level StatusLevel
	Mark  StatusLevel
}

// chip builds a reading whose colour and glyph agree.
func chip(label string, level StatusLevel) summaryChip {
	return summaryChip{Label: label, Level: level, Mark: level}
}

// yesNo grades a plain boolean, using fail when false.
func yesNo(label string, ok bool) summaryChip {
	if ok {
		return chip(label, LevelOK)
	}
	return chip(label, LevelFail)
}

// triChip grades a tri-state, leaving nil as an explicit "not determined"
// rather than folding it into "no".
func triChip(label string, v *bool) summaryChip {
	switch {
	case v == nil:
		return chip(label, LevelNeutral)
	case *v:
		return chip(label, LevelOK)
	default:
		return chip(label, LevelWarn)
	}
}

// connChips answers "what can this machine reach".
func connChips(th *Theme, r *netdiag.Report) []summaryChip {
	out := []summaryChip{
		yesNo(th.T(KDiagChipUDP), r.UDP.V4OK || r.UDP.V6OK),
		yesNo("IPv4", r.UDP.V4OK),
	}

	v6 := yesNo("IPv6", r.UDP.V6OK)
	if !r.UDP.V6OK {
		v6.Level = LevelNeutral
	}
	out = append(out, v6)

	out = append(out, chip(th.T(KDiagChipOverseas), diagLevel(r.Overseas.Status)))

	// DERP is tailscale's own reading; without it the row must say "unknown",
	// not "broken".
	derp := summaryChip{Label: "DERP", Level: LevelNeutral, Mark: LevelNeutral}
	if r.Tailscale.Available && r.Tailscale.PreferredDERP != "" {
		derp.Value = r.Tailscale.PreferredDERP
		derp.Level, derp.Mark = LevelOK, LevelOK
	}
	out = append(out, derp)

	// One egress address is the precondition for hole punching. A split seen
	// only over HTTP is a warning; a split STUN itself observed is a failure.
	egress := LevelOK
	switch {
	case r.Egress.DivergentSTUN:
		egress = LevelFail
	case r.Egress.Divergent:
		egress = LevelWarn
	}
	return append(out, chip(th.T(KDiagChipEgressOne), egress))
}

// envChips answers "what does the local network let us do".
func envChips(th *Theme, r *netdiag.Report) []summaryChip {
	nat := summaryChip{
		Label: natTypeLabel(th, r.NAT.Type),
		Level: diagLevel(r.NAT.Status),
		Mark:  diagLevel(r.NAT.Status),
	}
	out := []summaryChip{
		nat,
		yesNo(th.T(KDiagUPnP), r.PortMap.UPnP.Available),
		yesNo(th.T(KDiagNATPMP), r.PortMap.NATPMP.Available),
		yesNo(th.T(KDiagPCP), r.PortMap.PCP.Available),
		triChip(th.T(KDiagNatHairpin), r.NAT.Hairpin),
		triChip(th.T(KDiagNatPortPreserve), r.NAT.PortPreserving),
	}
	// A captive portal is an alarm, not a routine reading: show it only when
	// one was actually detected, so its presence is the signal.
	if r.Tailscale.CaptivePortal != nil && *r.Tailscale.CaptivePortal {
		out = append(out, chip(th.T(KDiagCaptivePortal), LevelFail))
	}
	return out
}

// chipRow lays readings out left to right, wrapping into the available width.
// This is the card body's answer to a KVList: the same facts, but packed across
// the card instead of one per line down an otherwise empty column.
func (p *diagPage) chipRow(a *App, gtx C, chips []summaryChip) D {
	th := a.th
	if len(chips) == 0 {
		return D{}
	}
	items := make([]layout.Widget, 0, len(chips))
	for _, c := range chips {
		items = append(items, func(gtx C) D {
			return layout.Inset{Right: SpaceSM, Bottom: 3}.Layout(gtx, func(gtx C) D {
				return th.Chip(gtx, ChipStyle{
					Text:  c.Label,
					Value: c.Value,
					Level: c.Level,
					Icon:  StatusIcon(c.Mark),
				})
			})
		})
	}
	return WrapRow(gtx, 3, items)
}

// measureWidth reports the width a widget wants, without drawing it.
//
// The recorded ops are discarded rather than replayed, so this costs one layout
// pass and paints nothing.
func measureWidth(gtx C, w layout.Widget) int {
	gtx.Constraints.Min = image.Point{}
	m := op.Record(gtx.Ops)
	dims := w(gtx)
	m.Stop()
	return dims.Size.X
}

// summaryLabelWidth is the shared width of the two group labels: the wider of
// them, so the rows line up and neither is ellipsised.
//
// A fixed width cannot work here. "连通性" is three glyphs and "Connectivity" is
// twelve, so any constant that keeps the Chinese layout tight truncates the
// English one — which is exactly what a hardcoded 52dp did.
func (p *diagPage) summaryLabelWidth(a *App, gtx C) int {
	th := a.th
	w := 0
	for _, k := range []Key{KDiagGroupConn, KDiagGroupEnv} {
		if n := measureWidth(gtx, th.Secondary(th.T(k)).Layout); n > w {
			w = n
		}
	}
	return w
}

// summaryRow renders a labelled chip row that wraps into the card's full width
// instead of running off the right edge.
func (p *diagPage) summaryRow(a *App, gtx C, labelW int, label string, chips []summaryChip) D {
	th := a.th
	if len(chips) == 0 {
		return D{}
	}
	return layout.Flex{Alignment: layout.Start}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = labelW, labelW
			return layout.Inset{Top: 4}.Layout(gtx, OneLine(th.Secondary(label)).Layout)
		}),
		HGap(SpaceSM),
		layout.Flexed(1, func(gtx C) D { return p.chipRow(a, gtx, chips) }),
	)
}

// hintList renders the "so what does that cost me" prose under a chip row.
//
// Only readings that are not OK get one. Packing the facts into chips is what
// buys the space back, but a chip has no room for the consequence, and the
// consequence is the part a non-expert actually needs — so it is kept exactly
// where it still earns its line and dropped where it only said "this is fine".
func (p *diagPage) hintList(a *App, gtx C, hints []string) D {
	th := a.th
	if len(hints) == 0 {
		return D{}
	}
	return layout.Inset{Top: SpaceSM}.Layout(gtx, func(gtx C) D {
		children := make([]layout.FlexChild, 0, len(hints))
		for _, h := range hints {
			children = append(children, layout.Rigid(func(gtx C) D {
				l := th.Caption("· " + h)
				l.MaxLines = 2
				return l.Layout(gtx)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

// triValueChip renders a tri-state as label + 是/否/未知.
func triValueChip(th *Theme, label string, v *bool) summaryChip {
	c := triChip(label, v)
	switch {
	case v == nil:
		c.Value = th.T(KUnknown)
	case *v:
		c.Value = th.T(KYes)
	default:
		c.Value = th.T(KNo)
	}
	return c
}

// behaviorLevel grades an RFC 5780 behaviour by what it costs hole punching.
func behaviorLevel(b netdiag.Behavior) StatusLevel {
	switch b {
	case netdiag.BehaviorEndpointIndependent:
		return LevelOK
	case netdiag.BehaviorAddressDependent:
		return LevelWarn
	case netdiag.BehaviorAddressAndPortDependent:
		return LevelFail
	default:
		return LevelNeutral
	}
}

// boolMark picks the glyph for a boolean whose colour has been softened. A
// false reading still earns a cross even when it is not painted as a problem.
func boolMark(v bool) StatusLevel {
	if v {
		return LevelOK
	}
	return LevelFail
}

// countLevel grades a "reachable / total" pair.
func countLevel(ok, total int) StatusLevel {
	switch {
	case total == 0:
		return LevelNeutral
	case ok == 0:
		return LevelFail
	case ok < total:
		return LevelWarn
	default:
		return LevelOK
	}
}

// ---------------------------------------------------------------------------
// Section cards
// ---------------------------------------------------------------------------

// sectionCard is the shared shell for a diagnostic section: title, verdict
// chip, and body.
func (p *diagPage) sectionCard(a *App, gtx C, icon IconFunc, title string, s netdiag.Status, summary string, body layout.Widget) D {
	th := a.th
	level := diagLevel(s)
	card := th.Card()
	card.Title = title
	card.Subtitle = summary
	card.Trailing = func(gtx C) D {
		return th.Chip(gtx, ChipStyle{Text: statusWord(th, s), Level: level, Dot: true})
	}
	return card.Layout(th, gtx, body)
}

func statusWord(th *Theme, s netdiag.Status) string {
	switch s {
	case netdiag.StatusOK:
		if th.Lang == LangZH {
			return "正常"
		}
		return "OK"
	case netdiag.StatusWarn:
		if th.Lang == LangZH {
			return "注意"
		}
		return "Warning"
	case netdiag.StatusFail:
		if th.Lang == LangZH {
			return "异常"
		}
		return "Failed"
	case netdiag.StatusSkipped:
		if th.Lang == LangZH {
			return "已跳过"
		}
		return "Skipped"
	default:
		return th.T(KUnknown)
	}
}

func natTypeLabel(th *Theme, t netdiag.NATType) string {
	switch t {
	case netdiag.NATOpen:
		return th.T(KNatOpen)
	case netdiag.NATFullCone:
		return th.T(KNatFullCone)
	case netdiag.NATRestricted:
		return th.T(KNatRestricted)
	case netdiag.NATPortRestrict:
		return th.T(KNatPortRestricted)
	case netdiag.NATSymmetric:
		return th.T(KNatSymmetric)
	case netdiag.NATUDPBlocked:
		return th.T(KNatUDPBlocked)
	case netdiag.NATSymmetricFW:
		return th.T(KNatSymmetricFW)
	default:
		return th.T(KNatUnknown)
	}
}

func behaviorLabel(th *Theme, b netdiag.Behavior) string {
	if th.Lang != LangZH {
		return b.String()
	}
	switch b {
	case netdiag.BehaviorEndpointIndependent:
		return "与目标无关"
	case netdiag.BehaviorAddressDependent:
		return "随目标地址变化"
	case netdiag.BehaviorAddressAndPortDependent:
		return "随目标地址和端口变化"
	default:
		return th.T(KUnknown)
	}
}

func (p *diagPage) triLabel(th *Theme, v *bool) (string, StatusLevel) {
	if v == nil {
		return th.T(KUnknown), LevelNeutral
	}
	if *v {
		return th.T(KYes), LevelOK
	}
	return th.T(KNo), LevelWarn
}

// A bare "unknown" or "no" in the results tells the user what was measured but
// not what it costs them. These helpers add the one-line consequence, which is
// the part that actually answers "should I care".
//
// They follow behaviorLabel's inline bilingual switch rather than i18n keys:
// the strings are explanatory prose, only ever used here.

// behaviorHint explains an RFC 5780 mapping/filtering behaviour.
func behaviorHint(th *Theme, b netdiag.Behavior) string {
	zh := th.Lang == LangZH
	switch b {
	case netdiag.BehaviorEndpointIndependent:
		if zh {
			return "对所有目标复用同一个外部端口，最利于打洞"
		}
		return "one external port for every destination — best case for hole punching"
	case netdiag.BehaviorAddressDependent:
		if zh {
			return "换一个目标地址就换一个映射"
		}
		return "the mapping changes with the destination address"
	case netdiag.BehaviorAddressAndPortDependent:
		if zh {
			return "目标地址或端口一变映射就变，等同对称型"
		}
		return "the mapping changes with address or port — effectively symmetric"
	default:
		if zh {
			return "没有服务器支持 CHANGE-REQUEST，无法判定"
		}
		return "no server supported CHANGE-REQUEST, so this could not be determined"
	}
}

// hairpinHint explains whether the NAT loops traffic sent to its own external
// address back inside.
func hairpinHint(th *Theme, v *bool) string {
	zh := th.Lang == LangZH
	switch {
	case v == nil:
		if zh {
			return "未测试"
		}
		return "not tested"
	case *v:
		if zh {
			return "同一内网的两台机器可经外网地址互连"
		}
		return "two machines behind this NAT can reach each other via the external address"
	default:
		if zh {
			return "同一内网内无法经外网地址回环，需走内网地址"
		}
		return "traffic to the external address does not loop back; use the LAN address instead"
	}
}

// preserveHint explains whether the external port matches the local one.
func preserveHint(th *Theme, v *bool) string {
	zh := th.Lang == LangZH
	switch {
	case v == nil:
		if zh {
			return "未测试"
		}
		return "not tested"
	case *v:
		if zh {
			return "外部端口与本地端口一致，对端更容易预测"
		}
		return "the external port matches the local one, so peers can predict it"
	default:
		if zh {
			return "外部端口被改写，端口预测不可靠"
		}
		return "the external port is rewritten, so port prediction is unreliable"
	}
}

// reachHint labels the CN/international pair, which is otherwise four bare
// numbers with no indication of what they count.
func reachHint(th *Theme) string {
	if th.Lang == LangZH {
		return "各自可达 / 探测总数"
	}
	return "reachable / probed, per region"
}

// udpFamilyStats counts responding and probed servers per address family.
// Probes whose DNS lookup failed carry no address and belong to neither.
func udpFamilyStats(r netdiag.UDPReport) (v4ok, v4n, v6ok, v6n int) {
	for _, p := range r.Probes {
		ap, err := netip.ParseAddrPort(p.Target)
		if err != nil {
			continue
		}
		if ap.Addr().Is4() || ap.Addr().Is4In6() {
			v4n++
			if p.OK {
				v4ok++
			}
			continue
		}
		v6n++
		if p.OK {
			v6ok++
		}
	}
	return
}

// udpFamilyHint reports how many servers answered on one address family.
func udpFamilyHint(th *Theme, ok, total int) string {
	zh := th.Lang == LangZH
	if total == 0 {
		if zh {
			return "没有可探测的地址"
		}
		return "no address to probe"
	}
	if zh {
		return itoa(ok) + "/" + itoa(total) + " 台服务器响应"
	}
	return itoa(ok) + "/" + itoa(total) + " servers responded"
}

func (p *diagPage) natCard(a *App, gtx C, r netdiag.NATReport) D {
	th := a.th
	_, hairpinLvl := p.triLabel(th, r.Hairpin)
	_, preserveLvl := p.triLabel(th, r.PortPreserving)

	return p.sectionCard(a, gtx, IconShield, th.T(KDiagSecNAT), r.Status, r.Summary, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			// The NAT type is the headline fact of this whole page; give it the
			// display size so it wins the visual hierarchy against the table.
			layout.Rigid(func(gtx C) D {
				return layout.Inset{Bottom: SpaceMD}.Layout(gtx, func(gtx C) D {
					l := th.Display(natTypeLabel(th, r.Type))
					l.Color = th.StatusColor(diagLevel(r.Status))
					return l.Layout(gtx)
				})
			}),
			layout.Rigid(func(gtx C) D {
				return p.chipRow(a, gtx, []summaryChip{
					{Label: th.T(KDiagNatMapping), Value: behaviorLabel(th, r.Mapping),
						Level: behaviorLevel(r.Mapping), Mark: behaviorLevel(r.Mapping)},
					{Label: th.T(KDiagNatFiltering), Value: behaviorLabel(th, r.Filtering),
						Level: behaviorLevel(r.Filtering), Mark: behaviorLevel(r.Filtering)},
					triValueChip(th, th.T(KDiagNatHairpin), r.Hairpin),
					triValueChip(th, th.T(KDiagNatPortPreserve), r.PortPreserving),
				})
			}),
			layout.Rigid(func(gtx C) D {
				var hints []string
				if behaviorLevel(r.Mapping) != LevelOK {
					hints = append(hints, th.T(KDiagNatMapping)+" — "+behaviorHint(th, r.Mapping))
				}
				if behaviorLevel(r.Filtering) != LevelOK {
					hints = append(hints, th.T(KDiagNatFiltering)+" — "+behaviorHint(th, r.Filtering))
				}
				if hairpinLvl != LevelOK {
					hints = append(hints, th.T(KDiagNatHairpin)+" — "+hairpinHint(th, r.Hairpin))
				}
				if preserveLvl != LevelOK {
					hints = append(hints, th.T(KDiagNatPortPreserve)+" — "+preserveHint(th, r.PortPreserving))
				}
				return p.hintList(a, gtx, hints)
			}),
			layout.Rigid(func(gtx C) D {
				if len(r.MappedAddrs) == 0 {
					return D{}
				}
				addrs := make([]string, 0, len(r.MappedAddrs))
				for _, ap := range r.MappedAddrs {
					addrs = append(addrs, ap.String())
				}
				return th.KV(gtx, KV{
					Key:   th.T(KDiagEgressIP),
					Value: strings.Join(addrs, "  "),
					Mono:  true,
					Level: mappedAddrLevel(len(r.MappedAddrs)),
				})
			}),
			layout.Rigid(func(gtx C) D {
				if len(r.Notes) == 0 {
					return D{}
				}
				return layout.Inset{Top: SpaceSM}.Layout(gtx, func(gtx C) D {
					children := make([]layout.FlexChild, 0, len(r.Notes))
					for _, n := range r.Notes {
						children = append(children, layout.Rigid(func(gtx C) D {
							l := th.Caption("· " + n)
							l.MaxLines = 3
							return l.Layout(gtx)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				})
			}),
			layout.Rigid(func(gtx C) D {
				if len(r.Results) == 0 {
					return D{}
				}
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					return p.stunTable(a, gtx, r.Results)
				})
			}),
		)
	})
}

func mappedAddrLevel(n int) StatusLevel {
	if n > 1 {
		return LevelWarn
	}
	return LevelNeutral
}

func (p *diagPage) stunTable(a *App, gtx C, results []netdiag.STUNResult) D {
	th := a.th
	children := make([]layout.FlexChild, 0, len(results)+1)
	children = append(children, layout.Rigid(func(gtx C) D {
		return p.tableHeader(a, gtx, "STUN", th.T(KDiagEgressIP), "RTT")
	}))
	for _, r := range results {
		children = append(children, layout.Rigid(func(gtx C) D {
			val, level := r.Mapped.String(), LevelOK
			if !r.OK {
				val, level = orDash(r.Err), LevelFail
			}
			rtt := ""
			if r.OK {
				rtt = FormatLatency(r.RTT)
			}
			name := r.Server
			if r.Name != "" {
				name = r.Name + " " + r.Server
			}
			return p.tableRow(a, gtx, regionTag(th, r.Region)+name, val, rtt, level)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// udpProbeLabel names a UDP probe the way the STUN table names its rows: the
// operator, then the hostname the user configured.
//
// The raw resolved address is not a useful label — nobody recognises
// 111.206.174.2:3478 as 小米 — but it is the only thing distinguishing the two
// rows a dual-stack server produces, so the family is appended instead.
func udpProbeLabel(pr netdiag.UDPProbe) string {
	host := pr.Host
	if host == "" {
		host = pr.Target
	}
	label := host
	if pr.Name != "" {
		label = pr.Name + " " + host
	}
	// Only meaningful when Target is a resolved address rather than a copy of
	// Host, which is what the DNS-failure path stores.
	if pr.Target != "" && pr.Target != pr.Host {
		if ap, err := netip.ParseAddrPort(pr.Target); err == nil {
			if ap.Addr().Is4() || ap.Addr().Is4In6() {
				label += " · IPv4"
			} else {
				label += " · IPv6"
			}
		}
	}
	return label
}

// regionTag prefixes a probe target so the CN/international split — the whole
// reason both are probed — is visible at a glance.
func regionTag(th *Theme, r netdiag.Region) string {
	if r == netdiag.RegionCN {
		if th.Lang == LangZH {
			return "[国内] "
		}
		return "[CN] "
	}
	if th.Lang == LangZH {
		return "[境外] "
	}
	return "[INTL] "
}

func (p *diagPage) tableHeader(a *App, gtx C, cols ...string) D {
	th := a.th
	return layout.Inset{Bottom: SpaceXS}.Layout(gtx, func(gtx C) D {
		return layout.Flex{}.Layout(gtx,
			layout.Flexed(0.44, func(gtx C) D {
				return OneLine(th.Caption(cols[0])).Layout(gtx)
			}),
			layout.Flexed(0.40, func(gtx C) D {
				return OneLine(th.Caption(cols[1])).Layout(gtx)
			}),
			layout.Flexed(0.16, func(gtx C) D {
				l := th.Caption(cols[2])
				l.Alignment = text.End
				return OneLine(l).Layout(gtx)
			}),
		)
	})
}

func (p *diagPage) tableRow(a *App, gtx C, left, mid, right string, level StatusLevel) D {
	th := a.th
	col := th.P.TextPri
	if level != LevelNeutral {
		col = th.StatusColor(level)
	}
	return layout.Inset{Top: 3, Bottom: 3}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(0.44, func(gtx C) D {
				return OneLine(th.Text(SizeCaption, th.P.TextSec, left)).Layout(gtx)
			}),
			layout.Flexed(0.40, func(gtx C) D {
				return OneLine(th.MonoLabel(SizeCaption, col, mid)).Layout(gtx)
			}),
			layout.Flexed(0.16, func(gtx C) D {
				l := th.MonoLabel(SizeCaption, th.P.TextDim, right)
				l.Alignment = text.End
				return OneLine(l).Layout(gtx)
			}),
		)
	})
}

func (p *diagPage) udpCard(a *App, gtx C, r netdiag.UDPReport) D {
	th := a.th
	v4, v4lvl := boolLabel(th, r.V4OK)
	v6, v6lvl := boolLabel(th, r.V6OK)
	// No IPv6 is normal on most Chinese home networks; flagging it red would
	// train the user to ignore the colour.
	if !r.V6OK {
		v6lvl = LevelNeutral
	}

	v4ok, v4n, v6ok, v6n := udpFamilyStats(r)

	return p.sectionCard(a, gtx, IconGlobe, th.T(KDiagSecUDP), r.Status, r.Summary, func(gtx C) D {
		cn, intl := "国内", "境外"
		if th.Lang != LangZH {
			cn, intl = "CN", "International"
		}
		chips := []summaryChip{
			{Label: th.T(KDiagUdpV4), Value: v4, Level: v4lvl, Mark: v4lvl},
			{Label: th.T(KDiagUdpV6), Value: v6, Level: v6lvl, Mark: boolMark(r.V6OK)},
			{Label: cn, Value: itoa(r.CNReachable) + "/" + itoa(r.CNTotal),
				Level: countLevel(r.CNReachable, r.CNTotal), Mark: countLevel(r.CNReachable, r.CNTotal)},
			{Label: intl, Value: itoa(r.IntlReachabl) + "/" + itoa(r.IntlTotal),
				Level: countLevel(r.IntlReachabl, r.IntlTotal), Mark: countLevel(r.IntlReachabl, r.IntlTotal)},
		}
		if len(r.BlockedPorts) > 0 {
			chips = append(chips, summaryChip{
				Label: th.T(KDiagUdpPortsBlocked), Value: joinInts(r.BlockedPorts),
				Level: LevelWarn, Mark: LevelWarn,
			})
		}
		var hints []string
		if !r.V4OK {
			hints = append(hints, th.T(KDiagUdpV4)+" — "+udpFamilyHint(th, v4ok, v4n))
		}
		if !r.V6OK {
			hints = append(hints, th.T(KDiagUdpV6)+" — "+udpFamilyHint(th, v6ok, v6n))
		}
		hints = append(hints, reachHint(th))
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D { return p.chipRow(a, gtx, chips) }),
			layout.Rigid(func(gtx C) D { return p.hintList(a, gtx, hints) }),
			layout.Rigid(func(gtx C) D {
				if len(r.Probes) == 0 {
					return D{}
				}
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					children := make([]layout.FlexChild, 0, len(r.Probes)+1)
					children = append(children, layout.Rigid(func(gtx C) D {
						return p.tableHeader(a, gtx, th.T(KDiagOverseasTarget), th.T(KDiagEgressIP), "RTT")
					}))
					for _, pr := range r.Probes {
						children = append(children, layout.Rigid(func(gtx C) D {
							val, level := pr.Mapped.String(), LevelOK
							rtt := FormatLatency(pr.RTT)
							if !pr.OK {
								val, level, rtt = orDash(pr.Err), LevelFail, ""
							}
							return p.tableRow(a, gtx, regionTag(th, pr.Region)+udpProbeLabel(pr), val, rtt, level)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				})
			}),
		)
	})
}

func boolLabel(th *Theme, v bool) (string, StatusLevel) {
	if v {
		return th.T(KYes), LevelOK
	}
	return th.T(KNo), LevelFail
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = itoa(n)
	}
	return strings.Join(parts, ", ")
}

func (p *diagPage) portMapCard(a *App, gtx C, r netdiag.PortMapReport) D {
	th := a.th
	return p.sectionCard(a, gtx, IconRouter, th.T(KDiagSecPortMap), r.Status, r.Summary, func(gtx C) D {
		chips := []summaryChip{
			serviceChip(th, th.T(KDiagUPnP), r.UPnP),
			serviceChip(th, th.T(KDiagNATPMP), r.NATPMP),
			serviceChip(th, th.T(KDiagPCP), r.PCP),
		}
		rows := []KV{}
		if r.Gateway.IsValid() {
			rows = append(rows, KV{Key: th.T(KDiagGateway), Value: r.Gateway.String(), Mono: true})
		}
		for _, s := range []netdiag.ServiceProbe{r.UPnP, r.NATPMP, r.PCP} {
			if s.ExternalIP.IsValid() {
				rows = append(rows, KV{
					Key:   th.T(KDiagExternalIP),
					Value: s.ExternalIP.String(),
					Mono:  true,
				})
				break
			}
		}
		// The per-service detail — router model, or why the probe failed — is
		// what makes this card actionable, so it survives the move to chips.
		var hints []string
		for _, s := range []struct {
			name  string
			probe netdiag.ServiceProbe
		}{
			{th.T(KDiagUPnP), r.UPnP},
			{th.T(KDiagNATPMP), r.NATPMP},
			{th.T(KDiagPCP), r.PCP},
		} {
			detail := s.probe.Detail
			if detail == "" {
				detail = s.probe.Err
			}
			if detail != "" {
				hints = append(hints, s.name+" — "+Truncate(detail, 70))
			}
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D { return p.chipRow(a, gtx, chips) }),
			layout.Rigid(func(gtx C) D { return p.hintList(a, gtx, hints) }),
			layout.Rigid(func(gtx C) D {
				if len(rows) == 0 {
					return D{}
				}
				return layout.Inset{Top: SpaceSM}.Layout(gtx, func(gtx C) D {
					return th.KVList(gtx, rows)
				})
			}),
		)
	})
}

func serviceChip(th *Theme, name string, s netdiag.ServiceProbe) summaryChip {
	val, level := th.T(KUnsupported), LevelWarn
	if s.Available {
		val, level = th.T(KSupported), LevelOK
	}
	return summaryChip{Label: name, Value: val, Level: level, Mark: level}
}

func (p *diagPage) overseasCard(a *App, gtx C, r netdiag.OverseasReport) D {
	th := a.th
	return p.sectionCard(a, gtx, IconGlobe, th.T(KDiagSecOverseas), r.Status, r.Summary, func(gtx C) D {
		if len(r.Probes) == 0 {
			return th.EmptyState(gtx, IconGlobe, th.T(KUnknown), "")
		}
		children := make([]layout.FlexChild, 0, len(r.Probes)+1)
		children = append(children, layout.Rigid(func(gtx C) D {
			return p.tableHeader(a, gtx, th.T(KDiagOverseasTarget), th.T(KDetails), "RTT")
		}))
		for _, pr := range r.Probes {
			children = append(children, layout.Rigid(func(gtx C) D {
				detail := itoa(pr.StatusCode)
				level := LevelOK
				if !pr.OK {
					detail, level = orDash(Truncate(pr.Err, 48)), LevelFail
				}
				via := "direct"
				if pr.ViaProxy {
					via = "proxy"
				}
				name := regionTag(th, pr.Region) + pr.URL + "  (" + via
				if pr.Network != "" {
					name += "/" + pr.Network
				}
				name += ")"
				return p.tableRow(a, gtx, name, detail, FormatLatency(pr.RTT), level)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (p *diagPage) egressCard(a *App, gtx C, r netdiag.EgressReport) D {
	th := a.th
	return p.sectionCard(a, gtx, IconGlobe, th.T(KDiagSecEgress), r.Status, r.Summary, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				if !r.Divergent {
					return D{}
				}
				// Only a split seen by STUN itself threatens the UDP path, so
				// only that one gets the red treatment.
				level, hint := LevelWarn, th.T(KDiagEgressDivergentHTTP)
				if r.DivergentSTUN {
					level, hint = LevelFail, th.T(KDiagEgressDivergentHint)
				}
				return layout.Inset{Bottom: SpaceMD}.Layout(gtx, func(gtx C) D {
					return p.callout(a, gtx, level, th.T(KDiagEgressDivergent), hint)
				})
			}),
			// Geolocation first: "where do I appear to be" is the question, the
			// per-probe table below is the evidence.
			layout.Rigid(func(gtx C) D {
				if len(r.Geo) == 0 {
					return D{}
				}
				children := make([]layout.FlexChild, 0, len(r.Geo))
				for _, g := range r.Geo {
					children = append(children, layout.Rigid(func(gtx C) D {
						return p.geoRow(a, gtx, g)
					}))
				}
				return layout.Inset{Bottom: SpaceMD}.Layout(gtx, func(gtx C) D {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				})
			}),
			layout.Rigid(func(gtx C) D {
				if len(r.Observations) == 0 {
					return th.EmptyState(gtx, IconGlobe, th.T(KUnknown), "")
				}
				children := make([]layout.FlexChild, 0, len(r.Observations)+1)
				children = append(children, layout.Rigid(func(gtx C) D {
					return p.tableHeader(a, gtx, th.T(KDiagEgressMethod), th.T(KDiagEgressIP), "RTT")
				}))
				for _, o := range r.Observations {
					children = append(children, layout.Rigid(func(gtx C) D {
						val, level := o.IP.String(), LevelOK
						rtt := FormatLatency(o.RTT)
						if !o.IP.IsValid() {
							val, level, rtt = orDash(Truncate(o.Err, 44)), LevelFail, ""
						}
						label := regionTag(th, o.Region) + string(o.Method) + " · " + o.Source
						return p.tableRow(a, gtx, label, val, rtt, level)
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			}),
		)
	})
}

func (p *diagPage) geoRow(a *App, gtx C, g netdiag.GeoInfo) D {
	th := a.th
	loc := []string{}
	for _, s := range []string{g.CountryName, g.Country, g.Region, g.City} {
		if s != "" && !containsStr(loc, s) {
			loc = append(loc, s)
		}
	}
	locText := strings.Join(loc, " · ")
	if locText == "" {
		locText = orDash(g.Err)
	}
	org := strings.TrimSpace(g.ASN + " " + g.Org)

	return layout.Inset{Top: 4, Bottom: 4}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return IconGlobe(gtx, gtx.Dp(15), th.P.Info)
			}),
			HGap(SpaceSM),
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(OneLine(th.MonoLabel(SizeBody, th.P.TextPri, g.IP.String())).Layout),
							HGap(SpaceSM),
							layout.Flexed(1, OneLine(th.Text(SizeBody, th.P.TextSec, locText)).Layout),
						)
					}),
					layout.Rigid(func(gtx C) D {
						if org == "" {
							return D{}
						}
						hint := org
						if g.Provider != "" {
							hint += "  ·  " + g.Provider
						}
						return OneLine(th.Caption(hint)).Layout(gtx)
					}),
				)
			}),
		)
	})
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func (p *diagPage) ifaceCard(a *App, gtx C, r netdiag.InterfaceReport) D {
	th := a.th
	return p.sectionCard(a, gtx, IconRoute, th.T(KDiagSecIface), r.Status, r.Summary, func(gtx C) D {
		rows := []KV{}
		if r.DefaultV4Src.IsValid() {
			rows = append(rows, KV{Key: th.T(KDiagIfaceDefaultV4), Value: r.DefaultV4Src.String(), Mono: true})
		}
		if r.DefaultV6Src.IsValid() {
			rows = append(rows, KV{Key: th.T(KDiagIfaceDefaultV6), Value: r.DefaultV6Src.String(), Mono: true})
		} else {
			rows = append(rows, KV{Key: th.T(KDiagIfaceDefaultV6), Value: th.T(KNone), Level: LevelNeutral})
		}

		children := []layout.FlexChild{
			layout.Rigid(func(gtx C) D { return th.KVList(gtx, rows) }),
		}
		if len(r.Addrs) > 0 {
			children = append(children, layout.Rigid(func(gtx C) D {
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					sub := make([]layout.FlexChild, 0, len(r.Addrs)+1)
					sub = append(sub, layout.Rigid(func(gtx C) D {
						return p.tableHeader(a, gtx, th.T(KPeerAddresses), "", "MTU")
					}))
					for _, ad := range r.Addrs {
						sub = append(sub, layout.Rigid(func(gtx C) D {
							name := ad.Iface
							if ad.IsDefaultSrc {
								name += " *"
							}
							mtu := ""
							if ad.MTU > 0 {
								mtu = itoa(ad.MTU)
							}
							return p.tableRow(a, gtx,
								name+"  "+string(ad.Kind), ad.Addr.String(), mtu,
								addrKindLevel(ad.Kind))
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, sub...)
				})
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func addrKindLevel(k netdiag.AddrKind) StatusLevel {
	switch k {
	case netdiag.AddrGlobalV4, netdiag.AddrGlobalV6:
		return LevelOK
	case netdiag.AddrTailscale:
		return LevelInfo
	case netdiag.AddrLoopback, netdiag.AddrLinkLocal:
		return LevelNeutral
	default:
		return LevelNeutral
	}
}

func (p *diagPage) tailscaleCard(a *App, gtx C, r netdiag.TailscaleReport) D {
	th := a.th
	return p.sectionCard(a, gtx, IconNodes, th.T(KDiagSecTailscale), r.Status, r.Summary, func(gtx C) D {
		if !r.Available {
			return th.EmptyState(gtx, IconNodes, orDash(r.Err), "")
		}
		portal, portalLvl := p.triLabel(th, r.CaptivePortal)
		if r.CaptivePortal != nil && *r.CaptivePortal {
			portalLvl = LevelFail
		} else if r.CaptivePortal != nil {
			portalLvl = LevelOK
		}

		rows := []KV{
			{Key: th.T(KDiagPreferredDERP), Value: orDash(r.PreferredDERP)},
			{Key: th.T(KDiagCaptivePortal), Value: portal, Level: portalLvl},
		}
		if r.GlobalV4 != "" {
			rows = append(rows, KV{Key: "GlobalV4", Value: r.GlobalV4, Mono: true})
		}
		if r.GlobalV6 != "" {
			rows = append(rows, KV{Key: "GlobalV6", Value: r.GlobalV6, Mono: true})
		}

		derp := append([]netdiag.DERPLatency(nil), r.DERP...)
		sort.Slice(derp, func(i, j int) bool { return derp[i].Latency < derp[j].Latency })
		if len(derp) > 6 {
			derp = derp[:6]
		}

		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D { return th.KVList(gtx, rows) }),
			layout.Rigid(func(gtx C) D {
				if len(derp) == 0 {
					return D{}
				}
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					children := make([]layout.FlexChild, 0, len(derp)+1)
					children = append(children, layout.Rigid(func(gtx C) D {
						return p.tableHeader(a, gtx, th.T(KDiagDerpLatency), "", "RTT")
					}))
					for _, d := range derp {
						children = append(children, layout.Rigid(func(gtx C) D {
							name := d.Name
							level := LevelNeutral
							if d.Preferred {
								name += "  ★"
								level = LevelOK
							}
							return p.tableRow(a, gtx, name, d.RegionCode,
								FormatLatency(d.Latency), level)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				})
			}),
		)
	})
}

// callout is an inline banner for a finding that needs a sentence of
// explanation rather than a table cell.
func (p *diagPage) callout(a *App, gtx C, level StatusLevel, title, body string) D {
	th := a.th
	col := th.StatusColor(level)
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			FillRRect(gtx, gtx.Constraints.Min, RadiusSM, WithAlpha(col, 0.10))
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.UniformInset(SpaceMD).Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Start}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return layout.Inset{Top: 2}.Layout(gtx, func(gtx C) D {
							return IconWarn(gtx, gtx.Dp(15), col)
						})
					}),
					HGap(SpaceSM),
					layout.Flexed(1, func(gtx C) D {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx C) D {
								l := th.Text(SizeBody, col, title)
								l.Font.Weight = font.Medium
								return l.Layout(gtx)
							}),
							layout.Rigid(func(gtx C) D {
								l := th.Text(SizeCaption, th.P.TextSec, body)
								l.MaxLines = 3
								return l.Layout(gtx)
							}),
						)
					}),
				)
			})
		}),
	)
}
