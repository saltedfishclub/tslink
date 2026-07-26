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

// getPeerFromRules maps every connect rule's destination onto the tailnet peer
// that carries it. Alongside the peers it reports how many rules could not be
// resolved at all; those are retryable, unlike destinations that resolve to an
// address outside the tailnet (an ordinary public host), which are skipped for
// good. warn selects whether unresolved rules are logged as warnings — during
// startup the tailnet resolver may not have its split-DNS routes yet, so the
// first few rounds stay quiet.
func getPeerFromRules(ctx context.Context, srv *tsnet.Server, rules map[string][]ConnectRule, logger *slog.Logger, warn bool) ([]netip.Addr, int) {
	peerSet := make(map[netip.Addr]struct{})
	unresolved := 0

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
				if errors.Is(err, errNotTailnetPeer) {
					logger.Debug("destination is outside the tailnet, skipping diagnostics",
						"tag", tag, "dst", rule.DstAddr, "err", err)
					continue
				}
				unresolved++
				if warn {
					logger.Warn("failed to resolve address", "tag", tag, "dst", rule.DstAddr, "err", err)
				} else {
					logger.Debug("failed to resolve address (tailnet DNS may still be settling)",
						"tag", tag, "dst", rule.DstAddr, "err", err)
				}
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
	return result, unresolved
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

const (
	// peerDiagInterval is how often connectivity to each peer is re-checked.
	peerDiagInterval = 120 * time.Second
	// A tsnet server reports Running before the netmap's DNS configuration has
	// been programmed into its resolver, and accept-routes is only applied once
	// the server is up — so at startup a split-DNS destination can briefly fail
	// to resolve even though it resolves fine moments later. Retry a handful of
	// times before reporting anything as broken.
	peerDiagWarmupTries = 6
	peerDiagWarmupDelay = 2 * time.Second
)

func StartPeerConnectivityDiagnostics(ctx context.Context, logger *slog.Logger, srv *tsnet.Server, rules map[string][]ConnectRule) {
	go func() {
		lc, err := srv.LocalClient()
		if err != nil {
			logger.Error("failed to get local client", "err", err)
			return
		}

		// Warm-up: keep retrying while destinations are still unresolvable, and
		// only escalate to a warning on the final attempt.
		var peers []netip.Addr
		for try := 1; ; try++ {
			last := try >= peerDiagWarmupTries
			var unresolved int
			peers, unresolved = getPeerFromRules(ctx, srv, rules, logger, last)
			if unresolved == 0 || last {
				break
			}
			logger.Debug("waiting for tailnet DNS before diagnosing peers",
				"unresolved", unresolved, "attempt", try)
			select {
			case <-ctx.Done():
				return
			case <-time.After(peerDiagWarmupDelay):
			}
		}
		logger.Debug("Peers loaded", "count", len(peers))

		ticker := time.NewTicker(peerDiagInterval)
		defer ticker.Stop()

		for {
			peerConnectivityLogic(ctx, lc, peers, logger)

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			// Re-resolve every round: destinations that failed at startup
			// recover on their own, and split-DNS records may point elsewhere
			// than they did two minutes ago.
			peers, _ = getPeerFromRules(ctx, srv, rules, logger, true)
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

func NormalizeDstAddrWithSuffix(ctx context.Context, srv *tsnet.Server, dst string) (string, bool, error) {
	host, port, err := net.SplitHostPort(dst)
	if err != nil {
		return dst, false, err
	}

	if _, err := netip.ParseAddr(host); err == nil {
		return dst, false, nil
	}

	suffix, ok := GetMagicDNSSuffix()
	if !ok {
		return dst, false, nil
	}

	qualified := host + "." + suffix
	normalized := net.JoinHostPort(qualified, port)

	// check domain exists before use. resolveAddr takes a bare host — passing
	// the "host:port" form made every lookup here fail on the stray colon.
	if strings.Contains(host, ".") {
		_, err = resolveAddr(ctx, srv, qualified)
		if err != nil {
			return dst, false, nil
		}
	}

	return normalized, true, nil
}

// normalizeDNSBudget caps how long the whole normalization pass may spend
// waiting on DNS. It runs before the connectors start listening, and on a cold
// start the tailnet resolver needs a few seconds before it answers — without a
// bound the listeners would not come up until then. A name that cannot be
// checked in time simply keeps its configured form, which is the same
// conclusion the check reaches for anything that is not a MagicDNS name.
const normalizeDNSBudget = 2 * time.Second

func NormalizeConnectRulesDstAddr(ctx context.Context, srv *tsnet.Server, rules map[string][]ConnectRule, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, normalizeDNSBudget)
	defer cancel()

	for tag, rrs := range rules {
		for i := range rrs {
			rule := &rrs[i]
			normalized, changed, err := NormalizeDstAddrWithSuffix(ctx, srv, rule.DstAddr)
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
