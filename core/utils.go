package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
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
		loLog := logger.With("peer", peer)

		ping, err := func() (*ipnstate.PingResult, error) {
			cnclCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			ping, err := lc.Ping(cnclCtx, peer, tailcfg.PingDisco)
			return ping, err
		}()

		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				loLog.Warn("connectivity: peer ping timeout")
			} else {
				loLog.Warn("connectivity: failed to ping peer", "err", err)
			}
			continue
		}

		peerInfo, err := lc.WhoIs(ctx, peer.String())
		if err != nil {
			loLog.Warn("failed to get peer info", "err", err)
		} else {
			loLog = loLog.With("name", peerInfo.Node.ComputedName)
		}

		var connect string
		if ping.DERPRegionCode == "" {
			connect = "direct"
		} else {
			connect = ping.DERPRegionCode
		}
		loLog.Info("connectivity: peer pinged",
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

func getSelfTsnetAddr(srv *tsnet.Server) netip.Addr {
	ip4, ip6 := srv.TailscaleIPs()
	ip := ip4
	if !ip.IsValid() {
		ip = ip6
	}
	return ip
}

var (
	magicDNSSuffixMu sync.RWMutex
	magicDNSSuffix   string
)

func SetMagicDNSSuffix(raw string) {
	magicDNSSuffixMu.Lock()
	defer magicDNSSuffixMu.Unlock()
	magicDNSSuffix = strings.Trim(raw, ".")
}

func GetMagicDNSSuffix() (string, bool) {
	magicDNSSuffixMu.RLock()
	defer magicDNSSuffixMu.RUnlock()
	if magicDNSSuffix == "" {
		return "", false
	}
	return magicDNSSuffix, true
}

func GetMagicDNSSuffixFromStatus(st *ipnstate.Status) (string, error) {
	suffix := st.CurrentTailnet.MagicDNSSuffix
	if suffix == "" {
		suffix = st.MagicDNSSuffix
	}
	suffix = strings.Trim(suffix, ".")
	if suffix == "" {
		return "", errors.New("magic dns suffix not found in status")
	}
	return suffix, nil
}

func NormalizeDstAddrWithSuffix(dst string) (string, bool, error) {
	host, port, err := net.SplitHostPort(dst)
	if err != nil {
		return dst, false, err
	}

	if _, err := netip.ParseAddr(host); err == nil {
		return dst, false, nil
	}

	if strings.Contains(host, ".") {
		return dst, false, nil
	}

	suffix, ok := GetMagicDNSSuffix()
	if !ok {
		return dst, false, nil
	}

	normalized := net.JoinHostPort(host+"."+suffix, port)
	return normalized, true, nil
}

func NormalizeConnectRulesDstAddr(rules map[string][]ConnectRule, logger *slog.Logger) {
	for tag, rrs := range rules {
		for i := range rrs {
			rule := &rrs[i]
			normalized, changed, err := NormalizeDstAddrWithSuffix(rule.DstAddr)
			if err != nil {
				logger.Debug("failed to normalize dst_addr",
					slog.String("tag", tag),
					slog.String("dst", rule.DstAddr),
					slog.String("error", err.Error()),
				)
				continue
			}
			if changed {
				logger.Debug("dst_addr normalized with MagicDNS suffix",
					slog.String("tag", tag),
					slog.String("original", rule.DstAddr),
					slog.String("normalized", normalized),
				)
				rule.DstAddr = normalized
			}
		}
	}
}
