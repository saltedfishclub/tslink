package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/dns"
	"tailscale.com/tsnet"
)

// magicdns suffix cache
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
	suffix = strings.Trim(suffix, ".")
	if suffix == "" {
		return "", errors.New("magic dns suffix not found in status")
	}
	return suffix, nil
}

// errNotTailnetPeer reports that a destination resolved successfully but the
// resulting IP is not carried by any tailnet peer — an ordinary public address.
// It is distinct from a resolution failure: retrying will not change the answer.
var errNotTailnetPeer = errors.New("address is not reachable through a tailnet peer")

// resolveAddr maps a destination host (an IP literal or a domain) to the
// tailnet address of the peer that carries it, so the peer can be pinged for
// connectivity diagnostics. Names are resolved through the same tailnet-aware
// path the dial code uses (see resolveHostToIP), so MagicDNS and split-DNS
// destinations behave identically in both.
func resolveAddr(ctx context.Context, srv *tsnet.Server, addr string) (*netip.Addr, error) {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		ip, err = resolveHostToIP(ctx, srv, addr)
		if err != nil {
			return nil, err
		}
	}

	stat, err := getCachedStatus(ctx, srv)
	if err != nil {
		return nil, err
	}

	peer, ok := peerCarryingIP(stat, ip)
	if !ok {
		return nil, fmt.Errorf("%w: %s (%s)", errNotTailnetPeer, addr, ip)
	}
	return &peer, nil
}

// peerCarryingIP returns the tailnet address of the peer that ip belongs to,
// either because it is the peer's own address or because the peer advertises a
// route covering it.
func peerCarryingIP(stat *ipnstate.Status, ip netip.Addr) (netip.Addr, bool) {
	for _, peer := range stat.Peer {
		for _, peerIP := range peer.TailscaleIPs {
			if peerIP == ip {
				return peer.TailscaleIPs[0], true
			}
		}
	}

	// Otherwise the subnet router advertising the most specific route wins.
	// Default routes are skipped: an exit node advertises 0.0.0.0/0, which
	// contains every address and would otherwise shadow the real owner at
	// random, since Go's map iteration order is unspecified. Ties are broken by
	// the lowest tailnet address so repeated calls agree with each other.
	bestBits := -1
	var best netip.Addr
	for _, peer := range stat.Peer {
		if peer.AllowedIPs == nil || peer.AllowedIPs.IsNil() || len(peer.TailscaleIPs) == 0 {
			continue
		}
		for _, route := range peer.AllowedIPs.All() {
			if route.Bits() == 0 || !route.Contains(ip) {
				continue
			}
			candidate := peer.TailscaleIPs[0]
			if route.Bits() > bestBits || (route.Bits() == bestBits && candidate.Compare(best) < 0) {
				bestBits, best = route.Bits(), candidate
			}
		}
	}
	return best, bestBits >= 0
}

// resolveHostToIP resolves a bare hostname to an address using the tailnet's
// own resolver. Both the dial path and the connectivity diagnostics go through
// here so they share one view of DNS.
//
// A bare single-label name additionally gets the MagicDNS suffix appended so
// short tailnet hostnames still resolve; a name that already contains a dot (an
// FQDN, including split-DNS suffixes) is queried as-is.
func resolveHostToIP(ctx context.Context, srv *tsnet.Server, host string) (netip.Addr, error) {
	dnsMgr, ok := srv.Sys().DNSManager.GetOK()
	if !ok {
		return netip.Addr{}, errors.New("DNS manager not available")
	}

	candidates := []string{host}
	if suffix, ok := GetMagicDNSSuffix(); ok && !strings.Contains(host, ".") {
		candidates = append(candidates, host+"."+suffix)
	}

	var lastErr error
	for _, name := range candidates {
		ip, err := resolveHostViaResolver(ctx, dnsMgr, name)
		if err != nil {
			lastErr = err
			continue
		}
		return ip, nil
	}
	return netip.Addr{}, fmt.Errorf("resolve %q via tailnet DNS: %w", host, lastErr)
}

// resolveDialAddr resolves the host portion of a "host:port" destination to a
// concrete "ip:port" using the tailnet's own DNS resolver.
//
// tsnet's Server.Dial only resolves MagicDNS names that are baked into the
// network map; for everything else it falls back to the host OS resolver, which
// has no knowledge of the tailnet's split-DNS configuration (custom search
// domains such as *.homelab.ice whose queries are routed to a nameserver
// reachable over Tailscale). By resolving through srv.Sys().DNSManager here —
// which honors MagicDNS and split-DNS routes exactly like quad-100 would — and
// dialing the resulting IP, split-DNS destinations resolve correctly.
//
// Literal IP destinations are returned unchanged. When tailnet resolution
// fails, the original address is returned together with the error so the caller
// may still fall back to dialing the name directly (e.g. via the system
// resolver for ordinary public names).
func resolveDialAddr(ctx context.Context, srv *tsnet.Server, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, err
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return addr, nil // already ip:port, nothing to resolve
	}

	ip, err := resolveHostToIP(ctx, srv, host)
	if err != nil {
		return addr, err
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// resolveHostViaResolver resolves a hostname to a netip.Addr using the
// Tailscale DNS resolver. It queries A then AAAA records and follows CNAME
// chains (up to 8 levels deep).
func resolveHostViaResolver(ctx context.Context, resolver *dns.Manager, host string) (netip.Addr, error) {
	return resolveHostWithDepth(ctx, resolver, host, 0)
}

func resolveHostWithDepth(ctx context.Context, r *dns.Manager, host string, depth int) (netip.Addr, error) {
	const maxCNAMEChase = 8
	if depth > maxCNAMEChase {
		return netip.Addr{}, fmt.Errorf("CNAME chain too deep for %s", host)
	}

	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid hostname %s: %w", host, err)
	}

	// Query A first (MagicDNS hands out an IPv4 for tailnet peers), then AAAA so
	// IPv6-only split-DNS hosts still resolve. A CNAME seen in either answer is
	// chased once no address record is found.
	var cnameTarget string
	for _, qType := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		ip, cname, err := queryResolver(ctx, r, name, qType)
		if err != nil {
			return netip.Addr{}, err
		}
		if ip.IsValid() {
			return ip, nil
		}
		if cname != "" {
			cnameTarget = cname
		}
	}

	// Follow CNAME if no direct address record was found.
	if cnameTarget != "" {
		return resolveHostWithDepth(ctx, r, cnameTarget, depth+1)
	}

	return netip.Addr{}, fmt.Errorf("no A/AAAA record found for %s", host)
}

// queryResolver sends a single question of the given type to the Tailscale DNS
// resolver and returns the first address answer, or a CNAME target if one is
// present instead.
func queryResolver(ctx context.Context, r *dns.Manager, name dnsmessage.Name, qType dnsmessage.Type) (netip.Addr, string, error) {
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{
			{Name: name, Type: qType, Class: dnsmessage.ClassINET},
		},
	}
	queryBytes, err := msg.Pack()
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("failed to pack DNS query: %w", err)
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	respBytes, err := r.Query(qctx, queryBytes, "udp", netip.AddrPort{})
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("DNS resolution failed for %s: %w", strings.TrimSuffix(name.String(), "."), err)
	}

	var resp dnsmessage.Message
	if err := resp.Unpack(respBytes); err != nil {
		return netip.Addr{}, "", fmt.Errorf("failed to unpack DNS response: %w", err)
	}

	var cname string
	for _, ans := range resp.Answers {
		switch body := ans.Body.(type) {
		case *dnsmessage.AResource:
			if ip := netip.AddrFrom4(body.A); ip.IsValid() {
				return ip, "", nil
			}
		case *dnsmessage.AAAAResource:
			if ip := netip.AddrFrom16(body.AAAA); ip.IsValid() {
				return ip, "", nil
			}
		case *dnsmessage.CNAMEResource:
			cname = strings.TrimSuffix(body.CNAME.String(), ".")
		}
	}
	return netip.Addr{}, cname, nil
}
