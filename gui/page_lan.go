package gui

import (
	"time"

	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"
)

type lanPage struct {
	list widget.List
	copy map[string]*widget.Clickable
}

func newLanPage() *lanPage {
	p := &lanPage{copy: make(map[string]*widget.Clickable)}
	p.list.Axis = layout.Vertical
	return p
}

func (p *lanPage) copyBtn(key string) *widget.Clickable {
	c, ok := p.copy[key]
	if !ok {
		c = &widget.Clickable{}
		p.copy[key] = c
	}
	return c
}

func (p *lanPage) Layout(a *App, gtx C, st core.State) D {
	th := a.th
	if st.Lan == nil {
		return th.EmptyState(gtx, IconBroadcast, th.T(KLanEmpty), th.T(KLoading))
	}

	servers := st.Lan.Servers()
	scanErr := st.Lan.Err()

	var advertised []core.LanEntry
	if st.Config != nil {
		advertised = core.LanEntriesFromRules(st.Config.Connect)
	}

	items := []layout.Widget{
		func(gtx C) D { return p.summaryCard(a, gtx, servers, advertised, scanErr) },
	}
	if len(servers) == 0 {
		items = append(items, func(gtx C) D {
			hint := th.T(KLanSubtitle)
			if scanErr != "" {
				hint = scanErr
			}
			return th.EmptyState(gtx, IconServer, th.T(KLanEmpty), hint)
		})
	}
	for _, s := range servers {
		items = append(items, func(gtx C) D { return p.serverCard(a, gtx, s) })
	}

	return material.List(th.Theme, &p.list).Layout(gtx, len(items), func(gtx C, i int) D {
		return layout.Inset{Bottom: SpaceMD}.Layout(gtx, items[i])
	})
}

// summaryCard states what the scanner is doing and, crucially, whether the
// advertisements tslink itself emits are being heard back. A rule that is
// configured but not audible means the tunnel or the multicast path is broken,
// and that is the single most useful thing this page can tell someone.
func (p *lanPage) summaryCard(a *App, gtx C, servers []core.LanServer, advertised []core.LanEntry, scanErr string) D {
	th := a.th
	selfHeard := 0
	for _, s := range servers {
		if s.IsSelf {
			selfHeard++
		}
	}
	missing := len(advertised) - selfHeard
	if missing < 0 {
		missing = 0
	}

	card := th.Card()
	card.Title = th.T(KLanTitle)
	card.Subtitle = th.T(KLanSubtitle)
	card.Trailing = func(gtx C) D {
		level, label := LevelOK, th.T(KLanListening)
		if scanErr != "" {
			level, label = LevelFail, th.T(KLanBindError)
		}
		return th.Chip(gtx, ChipStyle{Text: label, Level: level, Dot: true})
	}

	rows := []KV{
		{Key: th.T(KOvLanServers), Value: itoa(len(servers))},
		{Key: th.T(KLanSelf), Value: itoa(selfHeard) + " / " + itoa(len(advertised)),
			Hint:  th.T(KLanSelfHint),
			Level: selfLevel(len(advertised), selfHeard)},
	}
	if scanErr != "" {
		rows = append(rows, KV{Key: th.T(KError), Value: scanErr, Level: LevelFail})
	}
	return card.Layout(th, gtx, func(gtx C) D {
		return th.KVList(gtx, rows)
	})
}

func selfLevel(advertised, heard int) StatusLevel {
	switch {
	case advertised == 0:
		return LevelNeutral
	case heard >= advertised:
		return LevelOK
	case heard == 0:
		return LevelFail
	default:
		return LevelWarn
	}
}

func (p *lanPage) serverCard(a *App, gtx C, s core.LanServer) D {
	th := a.th
	addr := s.Addr.String() + ":" + itoa(s.Port)
	btn := p.copyBtn(addr)
	if btn.Clicked(gtx) {
		a.copyToClipboard(gtx, addr, "")
	}

	stale := time.Since(s.LastSeen) > 8*time.Second
	level := LevelOK
	if stale {
		level = LevelWarn
	}

	card := th.Card()
	card.Pad = SpaceMD
	if s.IsSelf {
		accent := th.P.Accent
		card.Accent = &accent
	}
	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return IconServer(gtx, gtx.Dp(18), th.StatusColor(level))
			}),
			HGap(SpaceMD),
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(OneLine(th.Body(orDash(s.Motd))).Layout),
							layout.Rigid(func(gtx C) D {
								if !s.IsSelf {
									return D{}
								}
								return layout.Inset{Left: SpaceSM}.Layout(gtx, func(gtx C) D {
									return th.Chip(gtx, ChipStyle{
										Text:  th.T(KLanSelf),
										Level: LevelInfo,
									})
								})
							}),
						)
					}),
					layout.Rigid(func(gtx C) D {
						return OneLine(th.MonoLabel(SizeCaption, th.P.TextSec, addr)).Layout(gtx)
					}),
				)
			}),
			HGap(SpaceMD),
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical, Alignment: layout.End}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return th.Text(SizeCaption, th.StatusColor(level),
							RelTime(th, s.LastSeen, time.Now())).Layout(gtx)
					}),
					layout.Rigid(func(gtx C) D {
						return th.Caption(itoa(s.Count) + " " + th.T(KLanPackets)).Layout(gtx)
					}),
				)
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				return th.IconButton(gtx, btn, IconCopy, LevelNeutral)
			}),
		)
	})
}
