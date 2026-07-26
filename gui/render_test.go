package gui

import (
	"image"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/unit"

	"tslink/core"
	"tslink/netdiag"
)

// These tests lay out every page without a GPU or a window.
//
// Layout is where a Gio UI actually breaks: a negative constraint, an
// unbounded flex child or a nil dereference in a rarely-taken branch panics at
// draw time, and there is no compiler check for any of it. Measurement runs
// the full flex/stack/text-shaping path, so exercising it catches those
// without needing a display — which also means it runs in CI.

func testTheme(t *testing.T) *Theme {
	t.Helper()
	// Skip system font discovery: CI images have no CJK font and the walk
	// would make the test depend on the host's font configuration.
	fonts := &FontSet{Collection: goCollection(), UI: "Go", Mono: "Go Mono"}
	th := NewTheme(fonts, true)
	th.Shaper = text.NewShaper(text.NoSystemFonts(), text.WithCollection(fonts.Collection))
	return th
}

// newTestContext builds a layout context backed by a real input router, so
// widgets that register event handlers behave as they do on screen.
func newTestContext(size image.Point) (layout.Context, *input.Router) {
	var r input.Router
	gtx := layout.Context{
		Ops:         new(op.Ops),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(size),
		Now:         time.Now(),
		Source:      r.Source(),
	}
	return gtx, &r
}

// testApp builds an App with no supervisor, which is the state the GUI is in
// before the service comes up.
func testApp(t *testing.T) *App {
	t.Helper()
	logs := core.NewLogBuffer(256)
	logger := slog.New(logs.Handler(nil))
	for i := 0; i < 40; i++ {
		logger.Info("synthetic log line", "i", i, "from", "test")
	}
	logger.Error("synthetic failure", "err", "boom", "from", "test")

	a := New(Options{
		Version:    "test",
		ConfigPath: "config.toml",
		Logs:       logs,
		Logger:     logger,
		StartDark:  true,
	})
	a.th = testTheme(t)
	return a
}

// readyState fabricates a running service with a peer, a LAN server and a
// diagnostic report, so the populated branches of every page get exercised
// rather than just the empty states.
func readyState(t *testing.T) core.State {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)

	cfg := &core.Config{
		Core: core.Core{Hostname: "test"},
		Connect: map[string][]core.ConnectRule{
			"survival": {{Protocol: "minecraft", LocalPort: 25565, DstAddr: "peer:25565"}},
		},
		Forward: map[string][]core.ForwardRule{
			"web": {{Protocol: "tcp", TailscalePort: 80, LocalAddr: "127.0.0.1:8080"}},
		},
	}

	return core.State{
		Phase:     core.PhaseReady,
		Steps:     nil,
		StartedAt: time.Now().Add(-time.Hour),
		ReadyAt:   time.Now().Add(-time.Hour),
		Config:    cfg,
		Peers:     core.NewPeerMonitor(nil, cfg.Connect, logger, core.PeerMonitorOptions{}),
	}
}

func TestPagesLayout(t *testing.T) {
	sizes := []image.Point{
		{X: 1200, Y: 800}, // roomy
		{X: 880, Y: 560},  // the declared minimum window
		{X: 640, Y: 400},  // below minimum: compact rail, everything must still fit
	}
	pages := []pageID{pageOverview, pagePeers, pageDiag, pageLogs, pageSettings}

	for _, size := range sizes {
		for _, page := range pages {
			a := testApp(t)
			a.current = page
			st := readyState(t)
			gtx, _ := newTestContext(size)
			// Two frames: the first registers widget state, the second takes
			// the paths that depend on it (hover, list position, caches).
			for i := 0; i < 2; i++ {
				a.shell(gtx, st)
			}
		}
	}
}

func TestSplashLayout(t *testing.T) {
	phases := []core.Phase{
		core.PhaseIdle, core.PhaseStarting, core.PhaseRetrying,
		core.PhaseError, core.PhaseStopped,
	}
	for _, phase := range phases {
		for _, size := range []image.Point{{X: 1200, Y: 800}, {X: 880, Y: 560}, {X: 700, Y: 380}} {
			a := testApp(t)
			st := core.State{
				Phase:     phase,
				Steps:     splashTestSteps(),
				StartedAt: time.Now().Add(-10 * time.Second),
				Err:       "core.auth_key is required",
			}
			gtx, _ := newTestContext(size)
			a.splash.Layout(a, gtx, st)
		}
	}
}

// TestSplashStuck covers the >20s branch, which swaps the footer hint and
// promotes the export button.
func TestSplashStuck(t *testing.T) {
	a := testApp(t)
	steps := splashTestSteps()
	for i := range steps {
		if steps[i].State == core.StepRunning {
			steps[i].Started = time.Now().Add(-45 * time.Second)
		}
	}
	st := core.State{
		Phase:     core.PhaseStarting,
		Steps:     steps,
		StartedAt: time.Now().Add(-45 * time.Second),
	}
	if !stalled(st) {
		t.Fatal("stalled() should report a step running past stuckAfter")
	}
	gtx, _ := newTestContext(image.Pt(460, 450))
	a.splash.Layout(a, gtx, st)
}

func splashTestSteps() []core.BootStep {
	now := time.Now()
	return []core.BootStep{
		{Key: core.StepKeyConfig, State: core.StepDone, Started: now.Add(-3 * time.Second), Finished: now.Add(-2 * time.Second)},
		{Key: core.StepKeyTsnet, State: core.StepRunning, Started: now.Add(-2 * time.Second)},
		{Key: core.StepKeyRules, State: core.StepPending},
		{Key: core.StepKeyServices, State: core.StepFailed, Err: "listen: address already in use"},
		{Key: core.StepKeyMonitors, State: core.StepSkipped},
		{Key: core.StepKeyReady, State: core.StepPending},
	}
}

// TestDiagPageWithReport renders every diagnostic section with a populated
// report, including the awkward cases: tri-state unknowns, divergent egress,
// and a failed probe row.
func TestDiagPageWithReport(t *testing.T) {
	a := testApp(t)
	a.current = pageDiag

	yes := true
	rep := &netdiag.Report{
		StartedAt: time.Now().Add(-20 * time.Second),
		Duration:  18 * time.Second,
		Status:    netdiag.StatusWarn,
		Headline:  "对称型 NAT：与同样受限的对端难以打洞",
		Interfaces: netdiag.InterfaceReport{
			Status:       netdiag.StatusOK,
			Summary:      "2 个接口 / 3 个地址",
			DefaultV4Src: netip.MustParseAddr("192.168.1.23"),
			Addrs: []netdiag.LocalAddr{
				{Iface: "eth0", Addr: netip.MustParseAddr("192.168.1.23"), Kind: netdiag.AddrPrivateV4, Up: true, MTU: 1500, IsDefaultSrc: true},
				{Iface: "tailscale0", Addr: netip.MustParseAddr("100.101.102.103"), Kind: netdiag.AddrTailscale, Up: true, MTU: 1280},
			},
		},
		UDP: netdiag.UDPReport{
			Status: netdiag.StatusWarn, Summary: "UDP 可用", V4OK: true,
			CNReachable: 3, CNTotal: 5, IntlReachabl: 1, IntlTotal: 7,
			BlockedPorts: []int{19302},
			Probes: []netdiag.UDPProbe{
				// A resolved probe: Host names the server, Target is the
				// address actually hit, and the label must show the former.
				{Host: "stun.miwifi.com:3478", Target: "111.206.174.2:3478", Name: "小米", Region: netdiag.RegionCN, OK: true, RTT: 12 * time.Millisecond, Mapped: netip.MustParseAddrPort("1.2.3.4:54321")},
				{Host: "stun.miwifi.com:3478", Target: "[2408::1]:3478", Name: "小米", Region: netdiag.RegionCN, OK: true, RTT: 15 * time.Millisecond, Mapped: netip.MustParseAddrPort("[2001:db8::9]:54321")},
				// DNS failed, so Target still holds the hostname.
				{Host: "stun.l.google.com:19302", Target: "stun.l.google.com:19302", Name: "Google", Region: netdiag.RegionIntl, Err: "i/o timeout"},
			},
		},
		NAT: netdiag.NATReport{
			Status: netdiag.StatusFail, Type: netdiag.NATSymmetric,
			Mapping: netdiag.BehaviorAddressAndPortDependent, Filtering: netdiag.BehaviorUnknown,
			Hairpin: nil, PortPreserving: &yes,
			MappedAddrs: []netip.AddrPort{netip.MustParseAddrPort("1.2.3.4:1"), netip.MustParseAddrPort("1.2.3.4:2")},
			Notes:       []string{"没有服务器支持 CHANGE-REQUEST"},
			Results: []netdiag.STUNResult{
				{Server: "stun.qq.com:3478", Name: "腾讯", Region: netdiag.RegionCN, OK: true, RTT: 9 * time.Millisecond, Mapped: netip.MustParseAddrPort("1.2.3.4:1")},
				{Server: "stun.cloudflare.com:3478", Region: netdiag.RegionIntl, Err: "no response"},
			},
		},
		PortMap: netdiag.PortMapReport{
			Status: netdiag.StatusWarn, Gateway: netip.MustParseAddr("192.168.1.1"),
			UPnP:   netdiag.ServiceProbe{Available: true, Detail: "Archer AX73 (TP-Link)", ExternalIP: netip.MustParseAddr("1.2.3.4")},
			NATPMP: netdiag.ServiceProbe{Err: "timeout"},
			PCP:    netdiag.ServiceProbe{Err: "timeout"},
		},
		Overseas: netdiag.OverseasReport{
			Status: netdiag.StatusWarn, Summary: "境外不可达",
			Probes: []netdiag.ReachProbe{
				{Name: "cf", URL: "https://cp.cloudflare.com/generate_204", Region: netdiag.RegionIntl, Network: "tcp4", Err: "timeout"},
				{Name: "baidu", URL: "https://www.baidu.com", Region: netdiag.RegionCN, OK: true, StatusCode: 200, RTT: 30 * time.Millisecond},
			},
		},
		Egress: netdiag.EgressReport{
			Status: netdiag.StatusWarn, Divergent: true, Summary: "出口 IP 不一致",
			UniqueIPs: []netip.Addr{netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("5.6.7.8")},
			Observations: []netdiag.EgressObservation{
				{Method: netdiag.MethodSTUN, Source: "stun.qq.com:3478", Region: netdiag.RegionCN, IP: netip.MustParseAddr("1.2.3.4")},
				{Method: netdiag.MethodHTTPProxy, Source: "https://api.ipify.org", Region: netdiag.RegionIntl, IP: netip.MustParseAddr("5.6.7.8")},
				{Method: netdiag.MethodHTTPv6, Source: "https://6.ipw.cn", Region: netdiag.RegionCN, Err: "no ipv6"},
			},
			Geo: []netdiag.GeoInfo{
				{IP: netip.MustParseAddr("1.2.3.4"), Country: "CN", City: "Shanghai", ASN: "AS4134", Org: "Chinanet", Provider: "ipinfo.io"},
				{IP: netip.MustParseAddr("5.6.7.8"), Err: "lookup failed"},
			},
			Countries: []string{"CN", "JP"},
		},
		Tailscale: netdiag.TailscaleReport{
			Available: true, UDP: true, IPv4: true, Status: netdiag.StatusOK,
			Summary: "首选 DERP tok", PreferredDERP: "tok",
			MappingVariesByDestIP: &yes,
			DERP: []netdiag.DERPLatency{
				{RegionID: 1, RegionCode: "tok", Name: "Tokyo", Latency: 40 * time.Millisecond, Preferred: true},
				{RegionID: 2, RegionCode: "sin", Name: "Singapore", Latency: 90 * time.Millisecond},
			},
		},
	}
	a.diag.report = rep
	a.diag.lastRun = time.Now()

	for _, size := range []image.Point{{X: 1200, Y: 800}, {X: 880, Y: 560}} {
		gtx, _ := newTestContext(size)
		st := readyState(t)
		for i := 0; i < 2; i++ {
			a.shell(gtx, st)
		}
	}

	// The report must also render as shareable text without panicking.
	if got := rep.Text(); got == "" {
		t.Fatal("Report.Text returned empty")
	}
}

func TestFormatHelpers(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{FormatLatency(0), "—"},
		{FormatLatency(1500 * time.Microsecond), "1.5 ms"},
		{FormatLatency(42 * time.Millisecond), "42 ms"},
		{FormatLatency(2500 * time.Millisecond), "2.5 s"},
		{FormatBytes(0), "0 B"},
		{FormatBytes(2048), "2 KiB"},
		{FormatBytes(5 * 1024 * 1024), "5 MiB"},
		{FormatDuration(90 * time.Second), "1m 30s"},
		{FormatDuration(3 * time.Hour), "3h 0m"},
		{Truncate("abcdef", 4), "abc…"},
		{Truncate("ab", 4), "ab"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %q want %q", i, c.got, c.want)
		}
	}
}

func TestTrFallsBackToEnglish(t *testing.T) {
	if Tr(LangEN, KNavPeers) != "Peers" {
		t.Errorf("english lookup failed")
	}
	if Tr(LangZH, KNavPeers) != "节点" {
		t.Errorf("chinese lookup failed")
	}
	if Tr(LangZH, Key(-1)) != "?" {
		t.Errorf("out-of-range key should not panic or return empty")
	}
	// Every key must resolve in both languages; a missing entry would render
	// as a bare "?" in the UI.
	for k := Key(0); k < kCount; k++ {
		if Tr(LangEN, k) == "?" {
			t.Errorf("key %d has no english string", k)
		}
		if Tr(LangZH, k) == "?" {
			t.Errorf("key %d has no chinese string", k)
		}
	}
}

// TestLayoutForceSplash exercises the top-level frame that runWindow drives.
// The splash window passes forceSplash=true; the shell window passes false.
// With no supervisor the state is not ready, so both must fall to the splash
// branch and lay out without panicking — the guard for the compile-time change
// to layout's signature and the forceSplash branch it added.
func TestLayoutForceSplash(t *testing.T) {
	a := testApp(t)
	for _, forceSplash := range []bool{true, false} {
		gtx, _ := newTestContext(image.Pt(int(shellWindowW), int(shellWindowH)))
		a.layout(gtx, forceSplash)
	}
}

// TestStatTilesUniformHeight guards the overview's top row. The tiles sit in a
// Flex, which does not equalise child heights, so anything that makes one tile
// taller — a wrapped value, a hint line present on some tiles but not others —
// visibly misaligns the row. Narrow widths are the interesting case: that is
// where "19 / 25" wraps and "8" does not.
func TestStatTilesUniformHeight(t *testing.T) {
	a := testApp(t)
	tiles := []struct{ value, label, hint string }{
		{"19 / 25", "在线节点", "3 已关联"},
		{"8", "本机服务", "5 已广播"},
		{"8 / 0", "连接规则 / 转发规则", ""},
		{"10s", "运行时长", ""},
	}
	for _, w := range []int{60, 80, 100, 140, 200, 300} {
		var first int
		for i, c := range tiles {
			gtx, _ := newTestContext(image.Pt(w, 400))
			gtx.Constraints.Min = image.Point{}
			h := a.overview.statTile(a, gtx, c.value, c.label, c.hint, LevelNeutral, IconNodes).Size.Y
			if i == 0 {
				first = h
				continue
			}
			if h != first {
				t.Errorf("width=%d: tile %q is %dpx, tile %q is %dpx — the row must be flush",
					w, c.label, h, tiles[0].label, first)
			}
		}
	}
}
