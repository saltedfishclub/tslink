package gui

import (
	"sort"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"
)

// chartWindow is the most latency history the graph shows. The monitor retains
// 20 minutes, but a spike that old tells you nothing about the session you are
// in right now, and stretching the axis over it flattens everything recent into
// noise. The axis scales to whatever data exists within this bound, so the plot
// is full from the second sample rather than after 20 minutes of uptime.
const chartWindow = 3 * time.Minute

// maxChartSeries caps how many peers are plotted at once. Beyond about eight
// lines a latency graph stops being readable, so linked peers win and the rest
// can be toggled on from the legend.
const maxChartSeries = 8

type peerRow struct {
	click    widget.Clickable
	expanded bool
}

type peersPage struct {
	list    widget.List
	chart   Chart
	rows    map[string]*peerRow
	legend  map[string]*widget.Clickable
	hidden  map[string]bool
	refresh widget.Clickable
}

func newPeersPage() *peersPage {
	p := &peersPage{
		rows:   make(map[string]*peerRow),
		legend: make(map[string]*widget.Clickable),
		hidden: make(map[string]bool),
	}
	p.list.Axis = layout.Vertical
	return p
}

func (p *peersPage) row(id string) *peerRow {
	r, ok := p.rows[id]
	if !ok {
		r = &peerRow{}
		p.rows[id] = r
	}
	return r
}

func (p *peersPage) legendClick(id string) *widget.Clickable {
	c, ok := p.legend[id]
	if !ok {
		c = &widget.Clickable{}
		p.legend[id] = c
	}
	return c
}

func (p *peersPage) Layout(a *App, gtx C, st core.State) D {
	th := a.th
	if st.Peers == nil {
		return th.EmptyState(gtx, IconNodes, th.T(KPeersEmpty), th.T(KLoading))
	}
	snap := st.Peers.Snapshot()

	if p.refresh.Clicked(gtx) {
		st.Peers.RefreshNow()
		a.notify(th.T(KRefresh), LevelInfo)
	}

	// Only nodes a config rule points at. The netmap contains every machine on
	// the tailnet, most of which the user has no rule for and no interest in.
	linked, _ := splitPeers(snap.Peers)
	series := p.buildSeries(th, linked)

	// Legend clicks toggle series visibility.
	for i := range series {
		id := series[i].id
		if p.legendClick(id).Clicked(gtx) {
			p.hidden[id] = !p.hidden[id]
		}
		series[i].s.Hidden = p.hidden[id]
	}

	items := make([]layout.Widget, 0, len(linked)+4)
	items = append(items, func(gtx C) D { return p.chartCard(a, gtx, series) })

	if len(linked) > 0 {
		items = append(items, func(gtx C) D {
			return a.sectionTitle(gtx, th.T(KPeersLinked), th.T(KGraphLegendHint), nil)
		})
		for _, pr := range linked {
			items = append(items, func(gtx C) D { return p.peerCard(a, gtx, st, pr) })
		}
	} else {
		items = append(items, func(gtx C) D {
			// Link resolution is periodic and needs DNS, so on a fresh boot
			// every peer is briefly unlinked. Saying "no peers" there would be
			// wrong; the netmap may be full of machines we simply have no rule
			// for yet.
			hint := snap.Err
			if hint == "" {
				hint = snap.BackendState
			}
			title := th.T(KPeersEmpty)
			if len(snap.Peers) > 0 {
				title = th.T(KPeersResolving)
			}
			return th.EmptyState(gtx, IconNodes, title, hint)
		})
	}

	return material.List(th.Theme, &p.list).Layout(gtx, len(items), func(gtx C, i int) D {
		return layout.Inset{Bottom: SpaceMD}.Layout(gtx, items[i])
	})
}

// splitPeers separates the peers a config rule points at from the rest. Those
// are the only ones whose latency actually matters to the user's game session.
func splitPeers(peers []core.PeerInfo) (linked, other []core.PeerInfo) {
	for _, p := range peers {
		if p.Linked {
			linked = append(linked, p)
		} else {
			other = append(other, p)
		}
	}
	return
}

type namedSeries struct {
	id string
	s  ChartSeries
}

func (p *peersPage) buildSeries(th *Theme, peers []core.PeerInfo) []namedSeries {
	candidates := append([]core.PeerInfo(nil), peers...)
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Linked != candidates[j].Linked {
			return candidates[i].Linked
		}
		return candidates[i].Online && !candidates[j].Online
	})

	out := make([]namedSeries, 0, maxChartSeries)
	for i, pr := range candidates {
		if len(out) >= maxChartSeries {
			break
		}
		if len(pr.Samples) == 0 {
			continue
		}
		// Drop samples outside the window by age rather than by count: the
		// monitor's ring is not evenly spaced, because a manual refresh injects
		// an off-cycle sweep.
		cutoff := time.Now().Add(-chartWindow)
		pts := make([]ChartPoint, 0, len(pr.Samples))
		for _, s := range pr.Samples {
			if s.At.Before(cutoff) {
				continue
			}
			pts = append(pts, ChartPoint{
				At:    s.At,
				Value: float64(s.Latency) / float64(time.Millisecond),
				OK:    s.OK,
			})
		}
		if len(pts) == 0 {
			continue
		}
		out = append(out, namedSeries{
			id: pr.ID,
			s: ChartSeries{
				Name:     pr.DisplayName,
				Color:    th.SeriesColor(i),
				Points:   pts,
				Subtitle: routeLabel(th, pr.Route),
			},
		})
	}
	return out
}

func (p *peersPage) chartCard(a *App, gtx C, series []namedSeries) D {
	th := a.th
	plot := make([]ChartSeries, len(series))
	for i, s := range series {
		plot[i] = s.s
	}
	style := ChartStyle{
		Height:     200,
		MaxWindow:  chartWindow,
		Now:        time.Now(),
		Unit:       "ms",
		FillSingle: true,
	}

	card := th.Card()
	card.Title = th.T(KGraphTitle)
	// The axis follows the data, so the subtitle has to as well — a fixed
	// "last 20 minutes" was a lie for the first 20 minutes of every run.
	if len(plot) > 0 {
		tMin, tMax := domain(plot, style)
		card.Subtitle = th.T(KGraphWindow) + " " + FormatDuration(tMax.Sub(tMin))
	}
	card.Trailing = func(gtx C) D {
		return th.IconButton(gtx, &p.refresh, IconRefresh, LevelNeutral)
	}
	return card.Layout(th, gtx, func(gtx C) D {
		if len(series) == 0 {
			return th.EmptyState(gtx, IconPulse, th.T(KGraphEmpty), "")
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return p.chart.Layout(th, gtx, style, plot)
			}),
			VGap(SpaceMD),
			layout.Rigid(func(gtx C) D {
				return p.legendRow(a, gtx, series)
			}),
		)
	})
}

func (p *peersPage) legendRow(a *App, gtx C, series []namedSeries) D {
	th := a.th
	entries := make([]LegendEntry, 0, len(series))
	for _, s := range series {
		entries = append(entries, LegendEntry{
			Name:   s.s.Name,
			Color:  s.s.Color,
			Hidden: p.hidden[s.id],
			Value:  lastValue(s.s.Points),
		})
	}
	return th.Legend(gtx, entries, func(i int) layout.Widget {
		click := p.legendClick(series[i].id)
		return func(gtx C) D {
			return click.Layout(gtx, func(gtx C) D {
				return th.LegendChip(gtx, entries[i], click.Hovered())
			})
		}
	})
}

func lastValue(points []ChartPoint) string {
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].OK {
			return FormatLatency(time.Duration(points[i].Value * float64(time.Millisecond)))
		}
	}
	return "—"
}

func routeLabel(th *Theme, r core.PeerRoute) string {
	switch r {
	case core.RouteDirect:
		return th.T(KPeerRouteDirect)
	case core.RouteDERP:
		return th.T(KPeerRouteDERP)
	case core.RoutePeerRelay:
		return th.T(KPeerRoutePeerRelay)
	case core.RouteOffline:
		return th.T(KPeerRouteOffline)
	default:
		return th.T(KPeerRouteUnknown)
	}
}

func routeLevel(r core.PeerRoute) StatusLevel {
	switch r {
	case core.RouteDirect:
		return LevelOK
	case core.RouteDERP, core.RoutePeerRelay:
		return LevelWarn
	case core.RouteOffline:
		return LevelFail
	default:
		return LevelNeutral
	}
}

func (p *peersPage) peerCard(a *App, gtx C, st core.State, pr core.PeerInfo) D {
	th := a.th
	row := p.row(pr.ID)
	if row.click.Clicked(gtx) {
		row.expanded = !row.expanded
	}

	card := th.Card()
	card.Pad = SpaceMD
	if pr.Linked {
		accent := th.SeriesColor(0)
		if !pr.Online {
			accent = th.P.TextDim
		}
		card.Accent = &accent
	}
	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return row.click.Layout(gtx, func(gtx C) D {
					return p.peerHeader(a, gtx, pr, row.expanded)
				})
			}),
			layout.Rigid(func(gtx C) D {
				if !row.expanded {
					return D{}
				}
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					return p.peerDetail(a, gtx, pr)
				})
			}),
		)
	})
}

func (p *peersPage) peerHeader(a *App, gtx C, pr core.PeerInfo, expanded bool) D {
	th := a.th
	level := LevelOK
	if !pr.Online {
		level = LevelNeutral
	}

	latency := "—"
	latLevel := LevelNeutral
	if pr.LatencyOK && pr.LastLatency > 0 {
		latency = FormatLatency(pr.LastLatency)
		latLevel = latencyLevel(pr.LastLatency)
	} else if pr.Online {
		latency = th.T(KUnknown)
	}

	points := make([]ChartPoint, 0, len(pr.Samples))
	for _, s := range pr.Samples {
		points = append(points, ChartPoint{
			At:    s.At,
			Value: float64(s.Latency) / float64(time.Millisecond),
			OK:    s.OK,
		})
	}

	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			// Never pulsing: one breathing dot per online peer would keep the
			// whole window redrawing for as long as the page is open.
			return th.StatusDot(gtx, level, false)
		}),
		HGap(SpaceMD),
		layout.Flexed(1, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							return OneLine(th.Body(pr.DisplayName)).Layout(gtx)
						}),
						layout.Rigid(func(gtx C) D {
							if !pr.Linked {
								return D{}
							}
							return layout.Inset{Left: 6}.Layout(gtx, func(gtx C) D {
								return IconLink(gtx, gtx.Dp(12), th.P.Accent)
							})
						}),
					)
				}),
				layout.Rigid(func(gtx C) D {
					sub := pr.DNSName
					if sub == "" && len(pr.TailscaleIPs) > 0 {
						sub = pr.TailscaleIPs[0].String()
					}
					if len(pr.LinkTags) > 0 {
						sub = strings.Join(pr.LinkTags, ", ") + " · " + sub
					}
					return OneLine(th.Caption(sub)).Layout(gtx)
				}),
			)
		}),
		HGap(SpaceMD),
		layout.Rigid(func(gtx C) D {
			return th.Sparkline(gtx, points, th.StatusColor(latLevel), 84, 22)
		}),
		HGap(SpaceMD),
		layout.Rigid(func(gtx C) D {
			gtx.Constraints.Min.X = gtx.Dp(70)
			l := th.MonoLabel(SizeBody, th.StatusColor(latLevel), latency)
			return l.Layout(gtx)
		}),
		HGap(SpaceSM),
		layout.Rigid(func(gtx C) D {
			return th.Chip(gtx, ChipStyle{
				Text:  routeLabel(th, pr.Route),
				Level: routeLevel(pr.Route),
			})
		}),
		HGap(SpaceSM),
		layout.Rigid(func(gtx C) D {
			icon := IconChevronRight
			if expanded {
				icon = IconChevronDown
			}
			return icon(gtx, gtx.Dp(14), th.P.TextDim)
		}),
	)
}

// latencyLevel colours a latency figure. The thresholds are chosen for the
// thing this tool carries: under 60ms a Minecraft session feels local, past
// 150ms block placement starts to feel wrong.
func latencyLevel(d time.Duration) StatusLevel {
	switch {
	case d <= 0:
		return LevelNeutral
	case d < 60*time.Millisecond:
		return LevelOK
	case d < 150*time.Millisecond:
		return LevelWarn
	default:
		return LevelFail
	}
}

func (p *peersPage) peerDetail(a *App, gtx C, pr core.PeerInfo) D {
	th := a.th
	addrs := make([]string, 0, len(pr.TailscaleIPs))
	for _, ip := range pr.TailscaleIPs {
		addrs = append(addrs, ip.String())
	}

	endpoint := pr.CurAddr
	if endpoint == "" {
		endpoint = pr.Relay
	}
	if endpoint == "" {
		endpoint = "—"
	}

	rows := []KV{
		{Key: th.T(KPeerAddresses), Value: strings.Join(addrs, "  "), Mono: true},
		{Key: th.T(KPeerEndpoint), Value: endpoint, Mono: true},
		{Key: th.T(KPeerOS), Value: orDash(pr.OS)},
		{
			Key: th.T(KPeerAvg) + " / " + th.T(KPeerMin) + " / " + th.T(KPeerMax),
			Value: FormatLatency(pr.AvgLatency) + "  " +
				FormatLatency(pr.MinLatency) + "  " + FormatLatency(pr.MaxLatency),
			Mono: true,
		},
		{
			Key:   th.T(KPeerJitter) + " / " + th.T(KPeerLoss),
			Value: trimZero(pr.JitterMs, 1) + " ms  ·  " + trimZero(pr.LossPct, 1) + " %",
			Mono:  true,
			Level: lossLevel(pr.LossPct),
		},
		{
			Key:   th.T(KPeerRx) + " / " + th.T(KPeerTx),
			Value: FormatBytes(pr.RxBytes) + "  ·  " + FormatBytes(pr.TxBytes),
			Mono:  true,
		},
		{Key: th.T(KPeerLastHandshake), Value: RelTime(th, pr.LastHandshake, time.Now())},
	}
	if !pr.Online {
		rows = append(rows, KV{
			Key:   th.T(KPeerLastSeen),
			Value: RelTime(th, pr.LastSeen, time.Now()),
			Level: LevelWarn,
		})
	}
	if pr.ExitNode {
		rows = append(rows, KV{Key: th.T(KPeerExitNode), Value: th.T(KYes), Level: LevelInfo})
	}
	return th.KVList(gtx, rows)
}

func lossLevel(pct float64) StatusLevel {
	switch {
	case pct <= 0:
		return LevelNeutral
	case pct < 5:
		return LevelWarn
	default:
		return LevelFail
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
