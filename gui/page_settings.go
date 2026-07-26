package gui

import (
	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"
)

type settingsPage struct {
	app  *App
	list widget.List

	theme   widget.Enum
	lang    widget.Enum
	restart widget.Clickable
}

func newSettingsPage(a *App) *settingsPage {
	p := &settingsPage{app: a}
	p.list.Axis = layout.Vertical
	p.theme.Value = "dark"
	if !a.th.Dark {
		p.theme.Value = "light"
	}
	p.lang.Value = "zh"
	if a.th.Lang == LangEN {
		p.lang.Value = "en"
	}
	return p
}

func (p *settingsPage) Layout(a *App, gtx C, st core.State) D {
	th := a.th

	if p.theme.Update(gtx) {
		th.SetDark(p.theme.Value == "dark")
	}
	if p.lang.Update(gtx) {
		if p.lang.Value == "en" {
			th.Lang = LangEN
		} else {
			th.Lang = LangZH
		}
	}
	if p.restart.Clicked(gtx) && a.opt.Supervisor != nil {
		a.opt.Supervisor.Restart()
		a.notify(th.T(KStateRetrying), LevelInfo)
	}

	items := []layout.Widget{
		func(gtx C) D { return p.appearanceCard(a, gtx) },
		func(gtx C) D { return p.aboutCard(a, gtx, st) },
	}
	return material.List(th.Theme, &p.list).Layout(gtx, len(items), func(gtx C, i int) D {
		return layout.Inset{Bottom: SpaceMD}.Layout(gtx, items[i])
	})
}

func (p *settingsPage) appearanceCard(a *App, gtx C) D {
	th := a.th
	card := th.Card()
	card.Title = th.T(KNavSettings)
	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return p.settingRow(a, gtx, th.T(KSetTheme), "", func(gtx C) D {
					return th.Segmented(gtx, &p.theme, []SegmentOption{
						{Key: "dark", Label: th.T(KSetThemeDark), Count: -1},
						{Key: "light", Label: th.T(KSetThemeLight), Count: -1},
					})
				})
			}),
			layout.Rigid(th.Divider),
			layout.Rigid(func(gtx C) D {
				hint := ""
				if !th.HasCJK {
					hint = th.T(KSetFontMissing)
				}
				return p.settingRow(a, gtx, th.T(KSetLanguage), hint, func(gtx C) D {
					return th.Segmented(gtx, &p.lang, []SegmentOption{
						{Key: "zh", Label: "中文", Count: -1},
						{Key: "en", Label: "English", Count: -1},
					})
				})
			}),
		)
	})
}

func (p *settingsPage) settingRow(a *App, gtx C, label, hint string, control layout.Widget) D {
	th := a.th
	return layout.Inset{Top: SpaceSM, Bottom: SpaceSM}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(th.Body(label).Layout),
					layout.Rigid(func(gtx C) D {
						if hint == "" {
							return D{}
						}
						return th.Caption(hint).Layout(gtx)
					}),
				)
			}),
			layout.Rigid(control),
		)
	})
}

func (p *settingsPage) aboutCard(a *App, gtx C, st core.State) D {
	th := a.th
	card := th.Card()
	card.Title = th.T(KSetAbout)
	card.Trailing = func(gtx C) D {
		return th.Button(gtx, &p.restart, ButtonStyle{
			Kind: ButtonSubtle, Text: th.T(KRetry), Icon: IconRefresh,
		})
	}

	cfgPath := a.opt.ConfigPath
	if a.opt.ConfigURL != "" {
		cfgPath = a.opt.ConfigURL
	}
	fontPath := a.fonts.CJKPath
	if fontPath == "" {
		fontPath = th.T(KNone)
	}

	return card.Layout(th, gtx, func(gtx C) D {
		rows := []KV{
			{Key: th.T(KSetVersion), Value: orDash(a.opt.Version), Mono: true},
			{Key: "Runtime", Value: runtimeInfo(), Mono: true},
			{Key: th.T(KSetConfigPath), Value: orDash(cfgPath), Mono: true},
			{Key: th.T(KSetFont), Value: fontPath, Mono: true},
			{Key: th.T(KStateRunning), Value: st.Phase.String()},
		}
		if st.Err != "" {
			rows = append(rows, KV{Key: th.T(KError), Value: st.Err, Level: LevelFail})
		}
		return th.KVList(gtx, rows)
	})
}
