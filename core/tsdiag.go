package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"tailscale.com/net/netcheck"
	"tailscale.com/net/netmon"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"

	"tslink/netdiag"
)

// TsDiagSource adapts a running tsnet server to [netdiag.TailscaleSource].
//
// The diagnostics package probes the network from scratch; this asks tailscale
// what it already believes. The two disagreeing is itself informative — for
// DERP latency, tailscale's answer is the one that governs how the tunnel will
// actually behave.
//
// netcheck's UPnP/PMP/PCP fields are not carried over: they are only populated
// when tailscale's port mapper has independently run, so reading them here
// yielded a permanent "unknown". netdiag probes those protocols directly.
type TsDiagSource struct {
	srv    *tsnet.Server
	logger *slog.Logger
}

// NewTailscaleSource wraps srv. A nil logger falls back to slog.Default.
func NewTailscaleSource(srv *tsnet.Server, logger *slog.Logger) *TsDiagSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &TsDiagSource{srv: srv, logger: logger}
}

// DefaultTailscaleSource returns a source for srv, or a nil interface when srv
// is nil, so callers can pass the result straight into netdiag.Options without
// tripping over a typed-nil interface.
func DefaultTailscaleSource(srv *tsnet.Server, logger *slog.Logger) netdiag.TailscaleSource {
	if srv == nil {
		return nil
	}
	return NewTailscaleSource(srv, logger)
}

// netcheckTimeout bounds one report. netcheck's own full run probes every DERP
// region, which takes a while on a slow link.
const netcheckTimeout = 15 * time.Second

// Netcheck runs tailscale's own network check and translates the result.
func (s *TsDiagSource) Netcheck(ctx context.Context) (rep *netdiag.TailscaleReport, err error) {
	if s == nil || s.srv == nil {
		return &netdiag.TailscaleReport{
			Status:  netdiag.StatusSkipped,
			Summary: "Tailscale 未运行",
		}, errors.New("tsnet server is nil")
	}

	lc, err := s.srv.LocalClient()
	if err != nil {
		return &netdiag.TailscaleReport{
			Status: netdiag.StatusSkipped,
			Err:    err.Error(),
		}, err
	}

	dm, err := lc.CurrentDERPMap(ctx)
	if err != nil || dm == nil {
		if err == nil {
			err = errors.New("no DERP map available")
		}
		return &netdiag.TailscaleReport{
			Status:  netdiag.StatusSkipped,
			Err:     err.Error(),
			Summary: "无法获取 DERP 列表，跳过 Tailscale 内部检查",
		}, err
	}

	// A static monitor takes a one-shot snapshot of the interfaces without
	// spawning the change-watching goroutines a long-lived Monitor would. That
	// is what we want for a single report, and Close on a static monitor is a
	// no-op.
	mon := netmon.NewStatic()

	client := &netcheck.Client{
		NetMon: mon,
		Logf: func(format string, args ...any) {
			s.logger.With(slog.String("from", "netcheck")).
				Debug(fmt.Sprintf(format, args...))
		},
	}

	runCtx, cancel := context.WithTimeout(ctx, netcheckTimeout)
	defer cancel()

	// GetReport reaches into internal magicsock machinery; a panic there must
	// degrade this one panel, not take the window down.
	var raw *netcheck.Report
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("netcheck panicked: %v", r)
			}
		}()
		raw, err = client.GetReport(runCtx, dm, &netcheck.GetReportOpts{})
	}()
	if err != nil || raw == nil {
		if err == nil {
			err = errors.New("netcheck returned no report")
		}
		return &netdiag.TailscaleReport{
			Status: netdiag.StatusSkipped,
			Err:    err.Error(),
		}, err
	}

	return convertNetcheck(raw, dm), nil
}

// convertNetcheck maps tailscale's report onto the diagnostics contract.
func convertNetcheck(raw *netcheck.Report, dm *tailcfg.DERPMap) *netdiag.TailscaleReport {
	out := &netdiag.TailscaleReport{
		Available: true,
		UDP:       raw.UDP,
		IPv4:      raw.IPv4,
		IPv6:      raw.IPv6,
		ICMPv4:    raw.ICMPv4,
		OSHasIPv6: raw.OSHasIPv6,

		CaptivePortal: optBool(raw.CaptivePortal.Get()),
	}
	if raw.GlobalV4.IsValid() {
		out.GlobalV4 = raw.GlobalV4.String()
	}
	if raw.GlobalV6.IsValid() {
		out.GlobalV6 = raw.GlobalV6.String()
	}

	for id, latency := range raw.RegionLatency {
		entry := netdiag.DERPLatency{
			RegionID:  id,
			Latency:   latency,
			Preferred: id == raw.PreferredDERP,
		}
		if dm != nil {
			if region, ok := dm.Regions[id]; ok && region != nil {
				entry.RegionCode = region.RegionCode
				entry.Name = region.RegionName
			}
		}
		if entry.RegionCode == "" {
			entry.RegionCode = strconv.Itoa(id)
		}
		if entry.Name == "" {
			entry.Name = entry.RegionCode
		}
		if entry.Preferred {
			out.PreferredDERP = entry.RegionCode
		}
		out.DERP = append(out.DERP, entry)
	}
	sort.Slice(out.DERP, func(i, j int) bool {
		if out.DERP[i].Latency != out.DERP[j].Latency {
			return out.DERP[i].Latency < out.DERP[j].Latency
		}
		return out.DERP[i].RegionID < out.DERP[j].RegionID
	})
	if out.PreferredDERP == "" && raw.PreferredDERP != 0 {
		out.PreferredDERP = strconv.Itoa(raw.PreferredDERP)
	}

	out.Status, out.Summary = netcheckVerdict(out)
	return out
}

// netcheckVerdict grades the report from the perspective of whether tailscale
// can carry traffic well, not whether every box is ticked.
func netcheckVerdict(r *netdiag.TailscaleReport) (netdiag.Status, string) {
	switch {
	case !r.UDP:
		return netdiag.StatusFail,
			"Tailscale 无法通过 UDP 与 DERP 通信，连接将非常不稳定"
	case r.CaptivePortal != nil && *r.CaptivePortal:
		return netdiag.StatusWarn,
			"检测到门户劫持（Captive Portal），需要先在浏览器完成网络认证"
	case len(r.DERP) == 0:
		return netdiag.StatusWarn,
			"没有任何 DERP 节点响应，中继回退可能不可用"
	}

	best := r.DERP[0]
	// Nothing here re-states the NAT verdict: netdiag.ClassifyNAT measures
	// mapping behaviour properly and owns that sentence.
	return netdiag.StatusOK, fmt.Sprintf("首选 DERP %s，延迟 %dms",
		nonEmpty(r.PreferredDERP, best.RegionCode),
		best.Latency.Milliseconds())
}

func nonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// optBool converts tailscale's opt.Bool (value, ok) pair into a tri-state
// pointer: nil means tailscale could not determine the answer, which is
// different from determining "no".
func optBool(v, ok bool) *bool {
	if !ok {
		return nil
	}
	out := v
	return &out
}
