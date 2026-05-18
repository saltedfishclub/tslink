package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

func StartTimeWatchDog(ctx context.Context, logger *slog.Logger) <-chan struct{} {
	logger.Info("starting watchdog")
	ch := make(chan struct{}, 1)
	go func() {
		lastUnix := time.Now().Unix()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				nowUnix := time.Now().Unix()
				diff := nowUnix - lastUnix
				if diff > 300 {
					logger.Warn("system time jump detected(wake up from sleep?)",
						slog.Int64("jump_seconds", diff),
					)
					ch <- struct{}{}
					logger.Debug("signal sent, watchdog exiting...")
					return
				}
				lastUnix = nowUnix
			}
		}
	}()
	return ch
}

func resolveAddr(ctx context.Context, srv *tsnet.Server, addr string) (*netip.Addr, error) {
	lc, err := srv.LocalClient()
	if err != nil {
		return nil, err
	}
	stat, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}

	if ip, err := netip.ParseAddr(addr); err == nil {
		for _, peer := range stat.Peer {
			for _, ipRange := range peer.AllowedIPs.All() {
				if ipRange.Contains(ip) {
					return &peer.TailscaleIPs[0], nil
				}
			}
		}
	} else {
		// addr is domain, resolve it
		for _, peer := range stat.Peer {
			dnsName := strings.TrimSuffix(peer.DNSName, ".")
			if dnsName == addr {
				return &peer.TailscaleIPs[0], nil
			}
		}
	}

	return nil, errors.New(fmt.Sprintf("addr '%s' not found in tsnet", addr))
}

func getPeerFromRules(ctx context.Context, srv *tsnet.Server, rules map[string][]ConnectRule, logger *slog.Logger) ([]netip.Addr, error) {
	peerSet := make(map[netip.Addr]struct{})

	for tag, rrs := range rules {
		for _, rule := range rrs {
			rule := rule
			tag := tag

			ap, _, err := net.SplitHostPort(rule.DstAddr)
			if err != nil {
				logger.Debug("error parsing rule", "tag", tag, "dst", rule.DstAddr, "err", err)
				// err log
				continue
			}
			addr, err := resolveAddr(ctx, srv, ap)

			if err != nil {
				logger.Warn("failed to resolve address", "err", err)
				continue
			}
			logger.Debug("address found", "dst_addr", rule.DstAddr, "tag", tag, "address", addr)
			peerSet[*addr] = struct{}{}
		}
	}

	var result []netip.Addr
	for peer := range peerSet {
		result = append(result, peer)
	}
	return result, nil
}

func peerConnectivityLogic(ctx context.Context, lc *local.Client, relativePeers []netip.Addr, logger *slog.Logger) {
	for _, peer := range relativePeers {
		ping, err := lc.Ping(ctx, peer, tailcfg.PingDisco)
		if err != nil {
			logger.Debug("connectivity: failed to ping peer", "peer", peer, "err", err)
			return
		}
		var connect string
		if ping.DERPRegionCode == "" {
			connect = "direct"
		} else {
			connect = ping.DERPRegionCode
		}
		logger.Info("connectivity: peer pinged",
			"peer", peer,
			"latency", fmt.Sprintf("%.2fms", ping.LatencySeconds*1000),
			"connect", connect,
		)
	}
}

func StartPeerConnectivityDiagnostics(ctx context.Context, logger *slog.Logger, srv *tsnet.Server, rules map[string][]ConnectRule) {
	relativePeers, err := getPeerFromRules(ctx, srv, rules, logger)
	if err != nil {
		return
	}
	logger.Debug("Peers loaded", "count", len(relativePeers))

	if len(relativePeers) == 0 {
		return
	}
	go func() {
		lc, err := srv.LocalClient()
		if err != nil {
			logger.Error("failed to get local client", "err", err)
			return
		}

		ticker := time.NewTicker(120 * time.Second)
		defer ticker.Stop()

		peerConnectivityLogic(ctx, lc, relativePeers, logger) // execute now

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				peerConnectivityLogic(ctx, lc, relativePeers, logger)
			}
		}
	}()
}
