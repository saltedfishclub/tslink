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

// addr(ip or domain) to tailscale ip
// check the address is in the tailscale network
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
		suffix, ok := GetMagicDNSSuffix()
		if ok {
			if !strings.HasSuffix(addr, suffix) {
				dnsMgr, ok := srv.Sys().DNSManager.GetOK()
				if !ok {
					return nil, errors.New("DNS manager not available")
				}
				ipaddr, err := resolveHostViaResolver(ctx, dnsMgr, addr)
				if err != nil {
					return nil, err
				}
				return resolveAddr(ctx, srv, ipaddr.String())
			}
		}
		// addr is tailscale domain, resolve it
		for _, peer := range stat.Peer {
			dnsName := strings.TrimSuffix(peer.DNSName, ".")
			if dnsName == addr {
				return &peer.TailscaleIPs[0], nil
			}
		}
	}

	return nil, errors.New(fmt.Sprintf("addr '%s' not found in tsnet", addr))
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

	dnsMgr, ok := srv.Sys().DNSManager.GetOK()
	if !ok {
		return addr, errors.New("DNS manager not available")
	}

	// Names to try, in order. A bare single-label name additionally gets the
	// MagicDNS suffix appended so short tailnet hostnames still resolve; a name
	// that already contains a dot (an FQDN, including split-DNS suffixes) is
	// queried as-is.
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
		return net.JoinHostPort(ip.String(), port), nil
	}
	return addr, fmt.Errorf("resolve %q via tailnet DNS: %w", host, lastErr)
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
