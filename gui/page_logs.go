package gui

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"tslink/core"
	"tslink/netdiag"
)

type logsPage struct {
	app  *App
	list widget.List

	search widget.Editor
	level  widget.Enum
	source widget.Enum
	follow widget.Bool
	redact widget.Bool

	copyBtn    widget.Clickable
	saveBtn    widget.Clickable
	uploadBtn  widget.Clickable
	urlCopyBtn widget.Clickable

	// mu guards the upload/save result fields, written from a goroutine.
	mu           sync.Mutex
	uploading    bool
	uploadURL    string
	uploadTarget string
	uploadErr    string
	savedPath    string

	// Cached filter result. Re-running the query over the whole ring on every
	// frame is wasted work: it can only change when a record is appended or
	// the query itself changes.
	cached    []core.LogEntry
	cachedSeq uint64
	cachedLen int
	cachedQ   core.LogQuery
}

// entries returns the filtered records, recomputing only when the buffer or
// the query moved.
func (p *logsPage) entries(buf *core.LogBuffer) []core.LogEntry {
	q := p.query()
	seq, n := buf.LastSeq(), buf.Len()
	if p.cached != nil && seq == p.cachedSeq && n == p.cachedLen && q == p.cachedQ {
		return p.cached
	}
	p.cached = buf.Filter(q)
	p.cachedSeq, p.cachedLen, p.cachedQ = seq, n, q
	return p.cached
}

func newLogsPage(a *App) *logsPage {
	p := &logsPage{app: a}
	p.list.Axis = layout.Vertical
	p.search.SingleLine = true
	p.level.Value = "all"
	p.source.Value = "all"
	p.follow.Value = true
	p.redact.Value = true
	return p
}

func (p *logsPage) minLevel() slog.Level {
	switch p.level.Value {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelDebug - 4 // below everything
	}
}

func (p *logsPage) query() core.LogQuery {
	q := core.LogQuery{
		MinLevel: p.minLevel(),
		Text:     strings.TrimSpace(p.search.Text()),
	}
	if p.source.Value != "all" {
		q.Source = p.source.Value
	}
	return q
}

func (p *logsPage) Layout(a *App, gtx C, st core.State) D {
	th := a.th
	if a.opt.Logs == nil {
		return th.EmptyState(gtx, IconList, th.T(KLogsEmpty), "")
	}
	buf := a.opt.Logs

	p.handleActions(a, gtx, buf)
	p.list.ScrollToEnd = p.follow.Value

	entries := p.entries(buf)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return layout.Inset{Bottom: SpaceMD}.Layout(gtx, func(gtx C) D {
				return p.toolbar(a, gtx, buf, len(entries))
			})
		}),
		layout.Flexed(1, func(gtx C) D {
			return p.logList(a, gtx, entries)
		}),
	)
}

func (p *logsPage) handleActions(a *App, gtx C, buf *core.LogBuffer) {
	th := a.th
	export := func() string {
		return buf.ExportText(core.ExportOptions{
			Query:    p.query(),
			NoRedact: !p.redact.Value,
			Header:   a.diagnosticHeader(),
		})
	}

	if p.copyBtn.Clicked(gtx) {
		a.copyToClipboard(gtx, export(), th.T(KCopied))
	}
	if p.saveBtn.Clicked(gtx) {
		path, err := saveLogFile(export())
		p.mu.Lock()
		if err != nil {
			p.savedPath = ""
			p.uploadErr = err.Error()
		} else {
			p.savedPath = path
			p.uploadErr = ""
		}
		p.mu.Unlock()
		if err != nil {
			a.notify(th.T(KError)+": "+err.Error(), LevelFail)
		} else {
			a.notify(path, LevelOK)
			a.reveal(path)
		}
	}
	if p.uploadBtn.Clicked(gtx) {
		p.startUpload(a, export())
	}
	if p.urlCopyBtn.Clicked(gtx) {
		p.mu.Lock()
		url := p.uploadURL
		p.mu.Unlock()
		if url != "" {
			a.copyToClipboard(gtx, url, th.T(KCopied))
		}
	}
}

// startUpload publishes the bundle to a public paste service.
//
// This sends the user's logs off the machine, so the redaction toggle is on by
// default and the button label says "upload and share" rather than something
// vaguer: nobody should be surprised about what just left their computer.
func (p *logsPage) startUpload(a *App, text string) {
	p.mu.Lock()
	if p.uploading {
		p.mu.Unlock()
		return
	}
	p.uploading = true
	p.uploadURL = ""
	p.uploadErr = ""
	p.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		res, err := netdiag.Upload(ctx, "", text, a.logger.With("from", "paste"))

		p.mu.Lock()
		p.uploading = false
		if err != nil {
			p.uploadErr = err.Error()
		} else {
			p.uploadURL = res.URL
			p.uploadTarget = res.Target
		}
		p.mu.Unlock()

		if err != nil {
			a.notify(a.th.T(KLogsUploadFail)+": "+Truncate(err.Error(), 80), LevelFail)
		} else {
			a.notify(a.th.T(KLogsUploaded)+" "+res.URL, LevelOK)
		}
		if a.win != nil {
			a.win.Invalidate()
		}
	}()
}

// saveLogFile writes the bundle next to the user's home directory. There is no
// native file picker without pulling in another dependency, so the app picks a
// predictable path and reports it rather than silently doing nothing.
func saveLogFile(content string) (string, error) {
	dir, err := os.UserHomeDir()
	if err != nil || dir == "" {
		dir = "."
	}
	name := "tslink-log-" + time.Now().Format("20060102-150405") + ".txt"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (p *logsPage) toolbar(a *App, gtx C, buf *core.LogBuffer, shown int) D {
	th := a.th
	counts := buf.Counts()
	total := buf.Len()

	card := th.Card()
	card.Pad = SpaceMD
	return card.Layout(th, gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			// Row 1: search + level filter.
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx C) D {
						return p.searchField(a, gtx)
					}),
					HGap(SpaceMD),
					layout.Rigid(func(gtx C) D {
						return th.Segmented(gtx, &p.level, []SegmentOption{
							{Key: "all", Label: th.T(KLogsAll), Count: total},
							{Key: "debug", Label: "DBG", Count: counts[slog.LevelDebug]},
							{Key: "info", Label: "INF", Count: counts[slog.LevelInfo]},
							{Key: "warn", Label: "WRN", Count: counts[slog.LevelWarn], Level: LevelWarn},
							{Key: "error", Label: "ERR", Count: counts[slog.LevelError], Level: LevelFail},
						})
					}),
				)
			}),
			// Row 2: source filter.
			layout.Rigid(func(gtx C) D {
				sources := buf.Sources()
				if len(sources) == 0 {
					return D{}
				}
				if len(sources) > 6 {
					sources = sources[:6]
				}
				opts := make([]SegmentOption, 0, len(sources)+1)
				opts = append(opts, SegmentOption{Key: "all", Label: th.T(KLogsAll), Count: -1})
				for _, s := range sources {
					opts = append(opts, SegmentOption{Key: s, Label: s, Count: -1})
				}
				return layout.Inset{Top: SpaceSM}.Layout(gtx, func(gtx C) D {
					return th.Segmented(gtx, &p.source, opts)
				})
			}),
			// Row 3: toggles + actions.
			layout.Rigid(func(gtx C) D {
				return layout.Inset{Top: SpaceMD}.Layout(gtx, func(gtx C) D {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							return th.Toggle(gtx, &p.follow, th.T(KLogsFollow))
						}),
						HGap(SpaceLG),
						layout.Rigid(func(gtx C) D {
							return th.Toggle(gtx, &p.redact, th.T(KLogsRedact))
						}),
						layout.Flexed(1, func(gtx C) D {
							return layout.E.Layout(gtx, func(gtx C) D {
								return p.actions(a, gtx)
							})
						}),
					)
				})
			}),
			// Row 4: counts + upload result.
			layout.Rigid(func(gtx C) D {
				return layout.Inset{Top: SpaceSM}.Layout(gtx, func(gtx C) D {
					return p.statusLine(a, gtx, buf, shown, total)
				})
			}),
		)
	})
}

func (p *logsPage) searchField(a *App, gtx C) D {
	th := a.th
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			FillRRect(gtx, gtx.Constraints.Min, RadiusSM, th.P.BgElevated)
			StrokeRRect(gtx, gtx.Constraints.Min, RadiusSM, 1, th.P.Border)
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{
				Top: 7, Bottom: 7, Left: SpaceMD, Right: SpaceMD,
			}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return IconSearch(gtx, gtx.Dp(14), th.P.TextDim)
					}),
					HGap(SpaceSM),
					layout.Flexed(1, func(gtx C) D {
						ed := material.Editor(th.Theme, &p.search, th.T(KLogsSearch))
						ed.TextSize = SizeBody
						ed.Color = th.P.TextPri
						ed.HintColor = th.P.TextDim
						return ed.Layout(gtx)
					}),
				)
			})
		}),
	)
}

func (p *logsPage) actions(a *App, gtx C) D {
	th := a.th
	p.mu.Lock()
	uploading := p.uploading
	p.mu.Unlock()

	uploadLabel := th.T(KLogsUpload)
	if uploading {
		uploadLabel = th.T(KLogsUploading)
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return th.Button(gtx, &p.copyBtn, ButtonStyle{
				Kind: ButtonGhost, Text: th.T(KLogsCopyAll), Icon: IconCopy,
			})
		}),
		HGap(SpaceSM),
		layout.Rigid(func(gtx C) D {
			return th.Button(gtx, &p.saveBtn, ButtonStyle{
				Kind: ButtonGhost, Text: th.T(KLogsSaveFile), Icon: IconSave,
			})
		}),
		HGap(SpaceSM),
		layout.Rigid(func(gtx C) D {
			return th.Button(gtx, &p.uploadBtn, ButtonStyle{
				Kind:     ButtonSubtle,
				Text:     uploadLabel,
				Icon:     IconUpload,
				Disabled: uploading,
			})
		}),
	)
}

func (p *logsPage) statusLine(a *App, gtx C, buf *core.LogBuffer, shown, total int) D {
	th := a.th
	p.mu.Lock()
	url, target, upErr, saved := p.uploadURL, p.uploadTarget, p.uploadErr, p.savedPath
	p.mu.Unlock()

	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			txt := th.T(KLogsShown) + " " + itoa(shown) + " / " + itoa(total)
			if d := buf.Dropped(); d > 0 {
				txt += "  ·  " + itoa(int(d)) + " " + th.T(KLogsDropped)
			}
			return th.Caption(txt).Layout(gtx)
		}),
		layout.Flexed(1, func(gtx C) D {
			return layout.E.Layout(gtx, func(gtx C) D {
				switch {
				case url != "":
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							return OneLine(th.MonoLabel(SizeCaption, th.P.OK, url)).Layout(gtx)
						}),
						layout.Rigid(func(gtx C) D {
							if target == "" {
								return D{}
							}
							return layout.Inset{Left: 6}.Layout(gtx,
								th.Caption("("+target+")").Layout)
						}),
						layout.Rigid(func(gtx C) D {
							return th.IconButton(gtx, &p.urlCopyBtn, IconCopy, LevelOK)
						}),
					)
				case upErr != "":
					return OneLine(th.Text(SizeCaption, th.P.Fail, Truncate(upErr, 90))).Layout(gtx)
				case saved != "":
					return OneLine(th.MonoLabel(SizeCaption, th.P.TextSec, saved)).Layout(gtx)
				default:
					return OneLine(th.Caption(th.T(KLogsRedactHint))).Layout(gtx)
				}
			})
		}),
	)
}

func (p *logsPage) logList(a *App, gtx C, entries []core.LogEntry) D {
	th := a.th
	card := th.Card()
	card.Pad = SpaceSM
	return card.Layout(th, gtx, func(gtx C) D {
		if len(entries) == 0 {
			return th.EmptyState(gtx, IconSearch, th.T(KLogsEmpty), "")
		}
		gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
		defer clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops).Pop()
		return material.List(th.Theme, &p.list).Layout(gtx, len(entries), func(gtx C, i int) D {
			return p.logRow(th, gtx, entries[i])
		})
	})
}

func (p *logsPage) logRow(th *Theme, gtx C, e core.LogEntry) D {
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

	msg := e.Msg
	attrs := make([]string, 0, len(e.Attrs))
	for _, at := range e.Attrs {
		if at.Key == "from" {
			continue
		}
		attrs = append(attrs, at.Key+"="+core.Redact(at.Value))
	}

	return layout.Inset{Top: 2, Bottom: 2, Left: SpaceSM, Right: SpaceSM}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Start}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return th.MonoLabel(SizeMono, WithAlpha(th.P.TextDim, 0.9),
					e.Time.Format("15:04:05.000")).Layout(gtx)
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Dp(28)
				return th.MonoLabel(SizeMono, lvlCol, core.LevelLabel(e.Level)).Layout(gtx)
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				if e.Source == "" {
					return D{}
				}
				gtx.Constraints.Max.X = gtx.Dp(96)
				l := th.MonoLabel(SizeMono, WithAlpha(th.P.Info, 0.85), e.Source)
				l.MaxLines = 1
				l.Alignment = text.End
				return l.Layout(gtx)
			}),
			HGap(SpaceSM),
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						l := th.MonoLabel(SizeMono, msgCol, core.Redact(msg))
						l.MaxLines = 3
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx C) D {
						if len(attrs) == 0 {
							return D{}
						}
						l := th.MonoLabel(SizeMono, WithAlpha(th.P.TextDim, 0.95),
							strings.Join(attrs, "  "))
						l.MaxLines = 2
						return l.Layout(gtx)
					}),
				)
			}),
		)
	})
}
