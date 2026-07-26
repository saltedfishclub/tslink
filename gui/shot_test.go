//go:build shots

package gui

import (
	"image"
	"image/png"
	"log/slog"
	"net/netip"
	"os"
	"testing"
	"time"

	"gioui.org/font"
	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"

	"tslink/core"
	"tslink/netdiag"
)

// Offscreen renders of the changed UI, for eyeballing what no assertion can
// capture — glyph coverage at bold weights, legend wrapping, how full the chart
// looks with only a few samples.
//
//	go test ./gui/ -tags shots -run TestShots
//
// Build-tagged so the normal suite stays GPU-free and font-config independent.

const shotDir = "/tmp/tslink-shots"

func shoot(t *testing.T, th *Theme, name string, size image.Point, w func(gtx C) D) {
	t.Helper()
	win, err := headless.NewWindow(size.X, size.Y)
	if err != nil {
		t.Skipf("no GPU backend: %v", err)
	}
	defer win.Release()

	var r input.Router
	ops := new(op.Ops)
	// Two frames: the second takes the paths that depend on widget state.
	for i := 0; i < 2; i++ {
		ops.Reset()
		gtx := layout.Context{
			Ops:         ops,
			Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
			Constraints: layout.Exact(size),
			Now:         time.Now(),
			Source:      r.Source(),
		}
		paint.Fill(gtx.Ops, th.P.Bg)
		w(gtx)
		if err := win.Frame(ops); err != nil {
			t.Fatalf("frame: %v", err)
		}
	}

	img := image.NewRGBA(image.Rectangle{Max: size})
	if err := win.Screenshot(img); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	f, err := os.Create(shotDir + "/" + name + ".png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s/%s.png", shotDir, name)
}

// realTheme builds the theme the way the app does, including the host's CJK
// font. Unlike testTheme this deliberately depends on the local font config —
// that is the thing under inspection.
func realTheme(t *testing.T) *Theme {
	t.Helper()
	fonts := LoadFonts()
	if !fonts.HasCJK {
		t.Skip("no CJK font on this host")
	}
	faces, err := LoadCJKFaces(fonts.CJKPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("cjk: %v", err)
	}
	fonts.Collection = append(fonts.Collection, faces...)
	th := NewTheme(fonts, true)
	th.Shaper = text.NewShaper(text.WithCollection(fonts.Collection))
	th.Lang = LangZH
	return th
}

func shotApp(t *testing.T, th *Theme) *App {
	a := testApp(t)
	a.th = th
	return a
}

func TestShots(t *testing.T) {
	if err := os.MkdirAll(shotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	th := realTheme(t)

	// --- 1. CJK at every weight the UI uses -------------------------------
	// The bug was that only weight 400 had a CJK face, so everything below
	// rendered as tofu boxes. All five lines must show Chinese glyphs.
	t.Run("cjk-weights", func(t *testing.T) {
		weights := []struct {
			w    font.Weight
			name string
		}{
			{font.Normal, "Normal 正文：延迟图谱 已关联 本机服务"},
			{font.Medium, "Medium 按钮：重试 导出日志 刷新"},
			{font.SemiBold, "SemiBold 标题：网络诊断 节点延迟"},
			{font.Bold, "Bold 强调：局域网 转发规则"},
		}
		shoot(t, th, "cjk-weights", image.Pt(560, 200), func(gtx C) D {
			return layout.UniformInset(SpaceLG).Layout(gtx, func(gtx C) D {
				children := make([]layout.FlexChild, 0, len(weights)*2)
				for _, w := range weights {
					children = append(children, layout.Rigid(func(gtx C) D {
						l := th.Text(SizeSubtitle, th.P.TextPri, w.name)
						l.Font.Weight = w.w
						return l.Layout(gtx)
					}), VGap(SpaceSM))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			})
		})
	})

	// --- 2. Splash, normal and stuck --------------------------------------
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{"splash", 10 * time.Second},
		{"splash-stuck", 45 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := shotApp(t, th)
			steps := splashTestSteps()
			for i := range steps {
				if steps[i].State == core.StepRunning {
					steps[i].Started = time.Now().Add(-tc.age)
				}
			}
			st := core.State{
				Phase: core.PhaseStarting, Steps: steps,
				StartedAt: time.Now().Add(-tc.age),
			}
			shoot(t, th, tc.name, image.Pt(460, 450), func(gtx C) D {
				return a.splash.Layout(a, gtx, st)
			})
		})
	}

	// --- 3. Chart + legend with 8 series and only 30s of history ----------
	// Previously this filled ~2.5% of the plot and clipped the legend.
	t.Run("chart", func(t *testing.T) {
		a := shotApp(t, th)
		p := a.peers
		series := p.buildSeries(th, shotPeers())
		shoot(t, th, "chart", image.Pt(760, 400), func(gtx C) D {
			return layout.UniformInset(SpaceLG).Layout(gtx, func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return p.chartCard(a, gtx, series)
			})
		})
	})

	// --- 3b. Stat tiles: equal height with and without a hint -------------
	t.Run("stat-tiles", func(t *testing.T) {
		a := shotApp(t, th)
		st := readyState(t)
		snap := core.PeerSnapshot{Peers: []core.PeerInfo{
			{ID: "n1", DisplayName: "a", Online: true, Linked: true},
		}}
		servers := buildServices(st.Config, snap)
		shoot(t, th, "stat-tiles", image.Pt(1000, 160), func(gtx C) D {
			return layout.UniformInset(SpaceLG).Layout(gtx, func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return a.overview.statRow(a, gtx, st, snap, servers)
			})
		})
	})

	// --- 4. Services card grouped per server ------------------------------
	t.Run("services", func(t *testing.T) {
		a := shotApp(t, th)
		cfg := &core.Config{Connect: map[string][]core.ConnectRule{
			"sfcraft":  {{Protocol: "minecraft", LocalPort: 25566, DstAddr: "sfcraft.mc.homelab.ice:25565", LanMotd: "SFCraft Vanilla | 原版生电 1.21.8"}},
			"mayday":   {{Protocol: "minecraft", LocalPort: 25571, DstAddr: "mayday.mc.homelab.ice:25565"}},
			"voice":    {{Protocol: "udp", LocalPort: 24454, DstAddr: "mc.lxns.net:24454"}},
			"l4d2_tcp": {{Protocol: "tcp", LocalPort: 27015, DstAddr: "server.l4d2.homelab.ice:27015"}},
			"l4d2_udp": {{Protocol: "udp", LocalPort: 27015, DstAddr: "server.l4d2.homelab.ice:27015"}},
		}}
		snap := core.PeerSnapshot{Peers: []core.PeerInfo{
			// One subnet router fronts every *.homelab.ice host, so they collapse
			// under a single tsdns-homelab header with the hosts nested beneath.
			{ID: "n1", DisplayName: "tsdns-homelab", Online: true, Linked: true,
				LinkTags: []string{"sfcraft", "mayday", "l4d2_tcp", "l4d2_udp"}},
		}}
		servers := buildServices(cfg, snap)
		shoot(t, th, "services", image.Pt(760, 480), func(gtx C) D {
			return layout.UniformInset(SpaceLG).Layout(gtx, func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return a.overview.servicesCard(a, gtx, servers)
			})
		})
	})
}

// TestShotsDiag renders the UDP table, which must name servers rather than
// print bare resolved addresses.
func TestShotsDiag(t *testing.T) {
	if err := os.MkdirAll(shotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	th := realTheme(t)
	a := shotApp(t, th)
	a.current = pageDiag
	st := readyState(t)
	a.diag.report = diagShotReport()
	shoot(t, th, "diag-udp", image.Pt(1120, 900), func(gtx C) D {
		return a.diag.Layout(a, gtx, st)
	})
}

// diagShotReport is a healthy report whose only complaint is an HTTP-only
// egress split — the case that must read as a yellow "may affect", not a red
// "is affecting".
func diagShotReport() *netdiag.Report {
	rep := &netdiag.Report{
		StartedAt: time.Now().Add(-18 * time.Second),
		Duration:  17 * time.Second,
		UDP: netdiag.UDPReport{
			Status: netdiag.StatusOK, Summary: "UDP 可用", V4OK: true,
			CNReachable: 2, CNTotal: 2, IntlReachabl: 1, IntlTotal: 2,
			Probes: []netdiag.UDPProbe{
				{Host: "stun.miwifi.com:3478", Target: "111.206.174.2:3478", Name: "小米",
					Region: netdiag.RegionCN, OK: true, RTT: 12 * time.Millisecond,
					Mapped: netip.MustParseAddrPort("1.2.3.4:54321")},
				{Host: "stun.miwifi.com:3478", Target: "[2408::1]:3478", Name: "小米",
					Region: netdiag.RegionCN, OK: true, RTT: 15 * time.Millisecond,
					Mapped: netip.MustParseAddrPort("[2001:db8::9]:54321")},
				{Host: "stun.chat.bilibili.com:3478", Target: "203.107.1.33:3478", Name: "哔哩哔哩",
					Region: netdiag.RegionCN, OK: true, RTT: 21 * time.Millisecond,
					Mapped: netip.MustParseAddrPort("1.2.3.4:54322")},
				{Host: "stun.l.google.com:19302", Target: "stun.l.google.com:19302", Name: "Google",
					Region: netdiag.RegionIntl, Err: "i/o timeout"},
			},
		},
		NAT: netdiag.NATReport{
			Status: netdiag.StatusOK, Type: netdiag.NATFullCone,
			Mapping:   netdiag.BehaviorEndpointIndependent,
			Filtering: netdiag.BehaviorUnknown,
		},
		Overseas: netdiag.OverseasReport{Status: netdiag.StatusOK, Summary: "境外可达"},
		Egress: netdiag.EgressReport{
			Observations: []netdiag.EgressObservation{
				{Method: netdiag.MethodSTUN, Source: "stun.miwifi.com:3478", IP: netip.MustParseAddr("1.2.3.4")},
				{Method: netdiag.MethodHTTPv4, Source: "https://example/ip", IP: netip.MustParseAddr("5.6.7.8")},
			},
		},
	}
	eg := &rep.Egress
	eg.UniqueIPs = []netip.Addr{netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("5.6.7.8")}
	eg.Divergent = true
	eg.DivergentSTUN = false
	eg.Status = netdiag.StatusWarn
	eg.Summary = "出口 IP 不一致：IPv4 有 2 个（1.2.3.4、5.6.7.8），仅 HTTP 探测存在差异"
	rep.Status = netdiag.StatusWarn
	rep.Headline = "仅 HTTP 探测到多个出口 IP，代理或分流工具可能影响连接"
	return rep
}

// shotPeers fabricates eight linked peers with ~30 seconds of history each —
// the short-uptime case the chart used to render almost entirely blank, and
// enough series to force the legend to wrap.
func shotPeers() []core.PeerInfo {
	now := time.Now()
	names := []string{
		"sfcraft-homelab", "mayday", "l4d2-server", "voice-relay",
		"mcp2-survival", "backup-node", "gateway-cn", "storage-nas",
	}
	peers := make([]core.PeerInfo, 0, len(names))
	for i, n := range names {
		var samples []core.PeerSample
		for k := 0; k < 4; k++ {
			samples = append(samples, core.PeerSample{
				At:      now.Add(time.Duration(-30+k*10) * time.Second),
				Latency: time.Duration(18+i*9+k*4) * time.Millisecond,
				OK:      true,
			})
		}
		peers = append(peers, core.PeerInfo{
			ID: n, DisplayName: n, Online: true, Linked: true,
			LinkTags: []string{n}, Route: core.RouteDirect,
			LastLatency: samples[len(samples)-1].Latency, LatencyOK: true,
			Samples: samples,
		})
	}
	return peers
}
