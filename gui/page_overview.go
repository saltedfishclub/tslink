package gui

import (
	"strings"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"
	"tslink/netdiag"
)

type overviewPage struct {
	list     widget.List
	diagBtn  widget.Clickable
	peersBtn widget.Clickable
	copySelf widget.Clickable
}

func newOverviewPage() *overviewPage {
	p := &overviewPage{}
	p.list.Axis = layout.Vertical
	return p
}

func (p *overviewPage) Layout(a *App, gtx C, st core.State) D {
	th := a.th

	if p.diagBtn.Clicked(gtx) {
		a.current = pageDiag
		a.diag.run()
	}
	if p.peersBtn.Clicked(gtx) {
		a.current = pagePeers
	}

	var snap core.PeerSnapshot
	if st.Peers != nil {
		snap = st.Peers.Snapshot()
	}
	var lanServers []core.LanServer
	if st.Lan != nil {
		lanServers = st.Lan.Servers()
	}

	if p.copySelf.Clicked(gtx) {
		a.copyToClipboard(gtx, selfAddrText(snap.Self), "")
	}

	items := []layout.Widget{
		func(gtx C) D { return p.statRow(a, gtx, st, snap, lanServers) },
		func(gtx C) D { return p.healthCard(a, gtx) },
		func(gtx C) D { return p.selfCard(a, gtx, st, snap) },
		func(gtx C) D { return p.linkedCard(a, gtx, snap) },
	}
	return material.List(th.Theme, &p.list).Layout(gtx, len(items), func(gtx C, i int) D {
		return layout.Inset{Bottom: SpaceMD}.Layout(gtx, items[i])
	})
}

// statTile is a headline number with its label. Four of them across the top
// answer "is anything obviously wrong" before the user reads anything else.
func (p *overviewPage) statTile(a *App, gtx C, value, label, hint string, level StatusLevel, icon IconFunc) D {
	th := a.th
	card := th.Card()
	card.Pad = SpaceLG
	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						if icon == nil {
							return D{}
						}
						return layout.Inset{Right: 6}.Layout(gtx, func(gtx C) D {
							return icon(gtx, gtx.Dp(13), th.P.TextDim)
						})
					}),
					layout.Flexed(1, OneLine(th.Caption(label)).Layout),
				)
			}),
			VGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				l := th.Display(value)
				if level != LevelNeutral {
					l.Color = th.StatusColor(level)
				}
				return l.Layout(gtx)
			}),
			layout.Rigid(func(gtx C) D {
				if hint == "" {
					return D{}
				}
				return OneLine(th.Caption(hint)).Layout(gtx)
			}),
		)
	})
}

func (p *overviewPage) statRow(a *App, gtx C, st core.State, snap core.PeerSnapshot, lan []core.LanServer) D {
	th := a.th

	online, linked := 0, 0
	for _, pr := range snap.Peers {
		if pr.Online {
			online++
		}
		if pr.Linked {
			linked++
		}
	}
	forwardRules, connectRules := 0, 0
	if st.Config != nil {
		for _, rs := range st.Config.Forward {
			forwardRules += len(rs)
		}
		for _, rs := range st.Config.Connect {
			connectRules += len(rs)
		}
	}
	selfLan := 0
	for _, s := range lan {
		if s.IsSelf {
			selfLan++
		}
	}

	peerLevel := LevelOK
	if len(snap.Peers) > 0 && online == 0 {
		peerLevel = LevelFail
	}

	uptime := "—"
	if !st.ReadyAt.IsZero() {
		uptime = FormatDuration(time.Since(st.ReadyAt))
	}

	tiles := []layout.Widget{
		func(gtx C) D {
			return p.statTile(a, gtx,
				itoa(online)+" / "+itoa(len(snap.Peers)),
				th.T(KOvPeersOnline),
				itoa(linked)+" "+th.T(KPeersLinked),
				peerLevel, IconNodes)
		},
		func(gtx C) D {
			return p.statTile(a, gtx,
				itoa(len(lan)),
				th.T(KOvLanServers),
				itoa(selfLan)+" "+th.T(KLanSelf),
				LevelNeutral, IconServer)
		},
		func(gtx C) D {
			return p.statTile(a, gtx,
				itoa(connectRules)+" / "+itoa(forwardRules),
				th.T(KOvConnectRules)+" / "+th.T(KOvForwardRules),
				"", LevelNeutral, IconLink)
		},
		func(gtx C) D {
			hint := ""
			if st.Restarts > 0 {
				hint = itoa(st.Restarts) + "×" + th.T(KStateRetrying)
			}
			return p.statTile(a, gtx, uptime, th.T(KOvUptime), hint, LevelNeutral, IconPulse)
		},
	}

	children := make([]layout.FlexChild, 0, len(tiles)*2-1)
	for i, t := range tiles {
		if i > 0 {
			children = append(children, HGap(SpaceMD))
		}
		children = append(children, layout.Flexed(1, t))
	}
	return layout.Flex{Alignment: layout.Start}.Layout(gtx, children...)
}

func (p *overviewPage) healthCard(a *App, gtx C) D {
	th := a.th
	a.diag.mu.Lock()
	rep := a.diag.report
	running := a.diag.running
	lastRun := a.diag.lastRun
	a.diag.mu.Unlock()

	card := th.Card()
	card.Title = th.T(KOvHealth)
	if rep != nil {
		card.Subtitle = th.T(KDiagLastRun) + " " + RelTime(th, lastRun, time.Now())
		accent := th.StatusColor(diagLevel(rep.Status))
		card.Accent = &accent
	}

	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx C) D {
				if rep == nil {
					return th.Secondary(th.T(KDiagNever)).Layout(gtx)
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						l := th.Text(SizeBody, th.StatusColor(diagLevel(rep.Status)), rep.Headline)
						l.Font.Weight = font.Medium
						l.MaxLines = 2
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx C) D {
						return layout.Inset{Top: SpaceSM}.Layout(gtx, func(gtx C) D {
							return p.healthChips(a, gtx, rep)
						})
					}),
				)
			}),
			HGap(SpaceMD),
			layout.Rigid(func(gtx C) D {
				label := th.T(KOvQuickDiag)
				if running {
					label = th.T(KDiagRunning)
				}
				return th.Button(gtx, &p.diagBtn, ButtonStyle{
					Kind:     ButtonPrimary,
					Text:     label,
					Icon:     IconPulse,
					Disabled: running,
				})
			}),
		)
	})
}

func (p *overviewPage) healthChips(a *App, gtx C, rep *netdiag.Report) D {
	th := a.th
	type chip struct {
		label string
		level StatusLevel
	}
	chips := []chip{
		{th.T(KDiagSecNAT) + ": " + natTypeLabel(th, rep.NAT.Type), diagLevel(rep.NAT.Status)},
		{th.T(KDiagSecUDP), diagLevel(rep.UDP.Status)},
		{th.T(KDiagSecOverseas), diagLevel(rep.Overseas.Status)},
		{th.T(KDiagSecPortMap), diagLevel(rep.PortMap.Status)},
		{th.T(KDiagSecEgress), diagLevel(rep.Egress.Status)},
	}
	children := make([]layout.FlexChild, 0, len(chips)*2)
	for i, c := range chips {
		if i > 0 {
			children = append(children, HGap(SpaceSM))
		}
		children = append(children, layout.Rigid(func(gtx C) D {
			return th.Chip(gtx, ChipStyle{Text: c.label, Level: c.level, Dot: true})
		}))
	}
	return layout.Flex{Spacing: layout.SpaceEnd}.Layout(gtx, children...)
}

func (p *overviewPage) selfCard(a *App, gtx C, st core.State, snap core.PeerSnapshot) D {
	th := a.th
	card := th.Card()
	card.Title = th.T(KOvSelf)
	card.Trailing = func(gtx C) D {
		return th.IconButton(gtx, &p.copySelf, IconCopy, LevelNeutral)
	}
	return card.Layout(th, gtx, func(gtx C) D {
		rows := []KV{
			{Key: th.T(KOvTailnet), Value: orDash(snap.TailnetName)},
			{Key: "Hostname", Value: orDash(snap.Self.DisplayName), Mono: true},
			{Key: th.T(KPeerAddresses), Value: orDash(selfAddrText(snap.Self)), Mono: true},
		}
		if snap.MagicDNSSuffix != "" {
			rows = append(rows, KV{Key: "MagicDNS", Value: snap.MagicDNSSuffix, Mono: true})
		}
		if snap.Err != "" {
			rows = append(rows, KV{Key: th.T(KError), Value: snap.Err, Level: LevelFail})
		}
		return th.KVList(gtx, rows)
	})
}

func selfAddrText(self core.PeerInfo) string {
	parts := make([]string, 0, len(self.TailscaleIPs))
	for _, ip := range self.TailscaleIPs {
		parts = append(parts, ip.String())
	}
	return strings.Join(parts, "  ")
}

func (p *overviewPage) linkedCard(a *App, gtx C, snap core.PeerSnapshot) D {
	th := a.th
	linked, _ := splitPeers(snap.Peers)

	card := th.Card()
	card.Title = th.T(KPeersLinked)
	card.Trailing = func(gtx C) D {
		return th.Button(gtx, &p.peersBtn, ButtonStyle{
			Kind: ButtonGhost, Text: th.T(KDetails), Icon: IconChevronRight,
		})
	}
	return card.Layout(th, gtx, func(gtx C) D {
		if len(linked) == 0 {
			return th.EmptyState(gtx, IconLink, th.T(KPeersEmpty), th.T(KOvConnectRules))
		}
		children := make([]layout.FlexChild, 0, len(linked)*2)
		for i, pr := range linked {
			if i > 0 {
				children = append(children, layout.Rigid(th.Divider))
			}
			children = append(children, layout.Rigid(func(gtx C) D {
				return p.linkedRow(a, gtx, pr)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (p *overviewPage) linkedRow(a *App, gtx C, pr core.PeerInfo) D {
	th := a.th
	level := LevelOK
	if !pr.Online {
		level = LevelFail
	}
	latency := "—"
	latLevel := LevelNeutral
	if pr.LatencyOK && pr.LastLatency > 0 {
		latency = FormatLatency(pr.LastLatency)
		latLevel = latencyLevel(pr.LastLatency)
	}
	points := make([]ChartPoint, 0, len(pr.Samples))
	for _, s := range pr.Samples {
		points = append(points, ChartPoint{
			At:    s.At,
			Value: float64(s.Latency) / float64(time.Millisecond),
			OK:    s.OK,
		})
	}

	return layout.Inset{Top: SpaceSM, Bottom: SpaceSM}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return th.StatusDot(gtx, level, false)
			}),
			HGap(SpaceMD),
			layout.Flexed(1, func(gtx C) D {
				return OneLine(th.Body(pr.DisplayName)).Layout(gtx)
			}),
			layout.Rigid(func(gtx C) D {
				return th.Sparkline(gtx, points, th.StatusColor(latLevel), 70, 18)
			}),
			HGap(SpaceMD),
			layout.Rigid(func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Dp(66)
				return th.MonoLabel(SizeBody, th.StatusColor(latLevel), latency).Layout(gtx)
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				return th.Chip(gtx, ChipStyle{
					Text:  routeLabel(th, pr.Route),
					Level: routeLevel(pr.Route),
				})
			}),
		)
	})
}
