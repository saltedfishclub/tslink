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

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/dns/resolver"
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

	for _, rrs := range rules {
		for _, rule := range rrs {
			rule := rule
			ap, err := netip.ParseAddrPort(rule.DstAddr)
			if err != nil {
				continue
			}
			peerSet[ap.Addr()] = struct{}{}
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

func PresolveDstAddrWithSuffix(dst string, srv *tsnet.Server) (string, bool, error) {
	host, port, err := net.SplitHostPort(dst)
	if err != nil {
		return dst, false, err
	}

	if _, err := netip.ParseAddr(host); err == nil {
		return dst, false, nil
	}

	dnsMgr, ok := srv.Sys().DNSManager.GetOK()
	if !ok {
		return dst, false, errors.New("DNS manager not available")
	}
	addr, err := resolveHostViaResolver(dnsMgr.Resolver(), host)
	if err != nil {
		// tsnet magicdns failed; fall back to system DNS for non-tailnet domains
		addr, err = fallbackSystemDNS(host)
		if err != nil {
			return dst, false, err
		}
	}
	return net.JoinHostPort(addr.String(), port), true, nil
}

// fallbackSystemDNS resolves a hostname via the standard system resolver.
// Returns the first usable IPv4 address (preferred) or IPv6 address.
func fallbackSystemDNS(host string) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("system DNS resolution failed for %s: %w", host, err)
	}

	for _, ip := range ips {
		if ip.Is4() {
			return ip, nil
		}
	}
	// no IPv4 found, pick the first IPv6
	for _, ip := range ips {
		if ip.Is6() {
			return ip, nil
		}
	}

	return netip.Addr{}, fmt.Errorf("no valid IPs returned for %s", host)
}

// resolveHostViaResolver resolves a hostname to a netip.Addr using the
// Tailscale DNS resolver. It queries A and AAAA records in a single
// message and follows CNAME chains (up to 8 levels deep).
func resolveHostViaResolver(resolver *resolver.Resolver, host string) (netip.Addr, error) {
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid hostname %s: %w", host, err)
	}

	msg := dnsmessage.Message{
		Header: dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{
			{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		},
	}
	queryBytes, err := msg.Pack()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to pack DNS query: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	respBytes, err := resolver.Query(ctx, queryBytes, "udp", netip.AddrPort{})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("DNS resolution failed for %s: %w", host, err)
	}

	var resp dnsmessage.Message
	if err := resp.Unpack(respBytes); err != nil {
		return netip.Addr{}, fmt.Errorf("failed to unpack DNS response: %w", err)
	}

	for _, ans := range resp.Answers {
		switch r := ans.Body.(type) {
		case *dnsmessage.AResource:
			if ip := netip.AddrFrom4(r.A); ip.IsValid() {
				return ip, nil
			}
		}
	}

	return netip.Addr{}, fmt.Errorf("no A/AAAA record found for %s", host)
}

func PresolveConnectRulesDstAddr(rules map[string][]ConnectRule, logger *slog.Logger, srv *tsnet.Server) {
	for tag, rrs := range rules {
		for i := range rrs {
			rule := &rrs[i]
			normalized, changed, err := PresolveDstAddrWithSuffix(rule.DstAddr, srv)
			if err != nil {
				logger.Warn("failed to resolve dst_addr",
					slog.String("tag", tag),
					slog.String("dst", rule.DstAddr),
					slog.String("error", err.Error()),
				)
				continue
			}
			if changed {
				logger.Debug("dst_addr resolved",
					slog.String("tag", tag),
					slog.String("original", rule.DstAddr),
					slog.String("normalized", normalized),
				)
				rule.DstAddr = normalized
			}
		}
	}
}
