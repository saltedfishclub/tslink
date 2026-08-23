package core

import (
	"context"
	"log/slog"
	"time"

	"tailscale.com/net/netcheck"
	"tailscale.com/net/netmon"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/util/eventbus"
)

const natCheckTimeout = 30 * time.Second

// StartNatTypeDetection probes the local network in the background using
// tailscale's netcheck (same machinery as `tailscale netcheck`) and logs
// the detected NAT type once the report is ready.
func StartNatTypeDetection(ctx context.Context, srv *tsnet.Server, logger *slog.Logger) {
	go func() {
		report, regionName, err := runNatCheck(ctx, srv, logger)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.With(slog.String("error", err.Error())).Warn("nat type detection failed")
			return
		}
		logNatReport(logger, report, regionName)
	}()
}

func runNatCheck(ctx context.Context, srv *tsnet.Server, slogger *slog.Logger) (*netcheck.Report, string, error) {
	ctx, cancel := context.WithTimeout(ctx, natCheckTimeout)
	defer cancel()

	lc, err := srv.LocalClient()
	if err != nil {
		return nil, "", err
	}
	dm, err := lc.CurrentDERPMap(ctx)
	if err != nil {
		return nil, "", err
	}

	bus := eventbus.New()
	defer bus.Close()
	netMon, err := netmon.New(bus, logger.Discard)
	if err != nil {
		return nil, "", err
	}
	defer netMon.Close()

	client := &netcheck.Client{
		NetMon: netMon,
		Logf:   logger.Discard,
	}
	if err := client.Standalone(ctx, ":0"); err != nil {
		slogger.With(slog.String("error", err.Error())).Debug("nat check udp bind partially failed")
	}

	report, err := client.GetReport(ctx, dm, nil)
	if err != nil {
		return nil, "", err
	}

	regionName := ""
	if region, ok := dm.Regions[report.PreferredDERP]; ok {
		regionName = region.RegionName
	}
	return report, regionName, nil
}

func logNatReport(logger *slog.Logger, report *netcheck.Report, derpRegion string) {
	natType := "unknown"
	switch {
	case !report.UDP:
		natType = "udp-blocked"
	default:
		if varies, ok := report.MappingVariesByDestIP.Get(); ok {
			if varies {
				natType = "symmetric (hard, endpoint-dependent mapping)"
			} else {
				natType = "cone (easy, endpoint-independent mapping)"
			}
		}
	}

	attrs := []any{
		slog.String("nat_type", natType),
		slog.Bool("udp", report.UDP),
		slog.Bool("ipv4", report.IPv4),
		slog.Bool("ipv6", report.IPv6),
	}
	if report.GlobalV4.IsValid() {
		attrs = append(attrs, slog.String("public_v4", report.GlobalV4.String()))
	}
	if report.GlobalV6.IsValid() {
		attrs = append(attrs, slog.String("public_v6", report.GlobalV6.String()))
	}
	if derpRegion != "" {
		attrs = append(attrs, slog.String("preferred_derp", derpRegion))
	}

	logger.With(attrs...).Info("nat type detected")

	if natType == "udp-blocked" {
		logger.Warn("UDP seems blocked, direct connections are unlikely; traffic will fall back to DERP relay")
	}
}
