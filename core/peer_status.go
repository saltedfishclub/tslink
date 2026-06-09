package core

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

var statusCache struct {
	mu      sync.Mutex
	status  *ipnstate.Status
	expires time.Time
}

const statusCacheTTL = 5 * time.Second

func getCachedStatus(ctx context.Context, srv *tsnet.Server) (*ipnstate.Status, error) {
	statusCache.mu.Lock()
	if statusCache.status != nil && time.Now().Before(statusCache.expires) {
		st := statusCache.status
		statusCache.mu.Unlock()
		return st, nil
	}
	statusCache.mu.Unlock()

	lc, err := srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}

	statusCache.mu.Lock()
	statusCache.status = st
	statusCache.expires = time.Now().Add(statusCacheTTL)
	statusCache.mu.Unlock()
	return st, nil
}

func isTsnetTarget(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		tsnetV4 := netip.MustParsePrefix("100.64.0.0/10")
		tsnetV6 := netip.MustParsePrefix("fd7a:115c:a1e0::/48")
		return tsnetV4.Contains(ip) || tsnetV6.Contains(ip)
	}
	return true
}

func getConnType(ctx context.Context, srv *tsnet.Server, remoteAddrStr string) string {
	st, err := getCachedStatus(ctx, srv)
	if err != nil {
		return "unknown"
	}

	remoteHost, _, err := net.SplitHostPort(remoteAddrStr)
	if err != nil {
		return "unknown"
	}

	for _, peer := range st.Peer {
		for _, addr := range peer.TailscaleIPs {
			if addr.String() == remoteHost {
				if peer.CurAddr != "" {
					return "direct"
				}
				if peer.Relay != "" {
					return fmt.Sprintf("derp(%s)", peer.Relay)
				}
				return "direct"
			}
		}
	}
	return "unknown"
}
