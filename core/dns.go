package core

import (
	"context"
	"errors"
	"fmt"
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
				ipaddr, err := resolveHostViaResolver(dnsMgr, addr)
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

// resolveHostViaResolver resolves a hostname to a netip.Addr using the
// Tailscale DNS resolver. It queries A and AAAA records in a single
// message and follows CNAME chains (up to 8 levels deep).
func resolveHostViaResolver(resolver *dns.Manager, host string) (netip.Addr, error) {
	return resolveHostWithDepth(resolver, host, 0)
}

func resolveHostWithDepth(r *dns.Manager, host string, depth int) (netip.Addr, error) {
	const maxCNAMEChase = 8
	if depth > maxCNAMEChase {
		return netip.Addr{}, fmt.Errorf("CNAME chain too deep for %s", host)
	}

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
	respBytes, err := r.Query(ctx, queryBytes, "udp", netip.AddrPort{})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("DNS resolution failed for %s: %w", host, err)
	}

	var resp dnsmessage.Message
	if err := resp.Unpack(respBytes); err != nil {
		return netip.Addr{}, fmt.Errorf("failed to unpack DNS response: %w", err)
	}

	var cnameTarget string
	for _, ans := range resp.Answers {
		switch r := ans.Body.(type) {
		case *dnsmessage.AResource:
			if ip := netip.AddrFrom4(r.A); ip.IsValid() {
				return ip, nil
			}
		case *dnsmessage.CNAMEResource:
			cnameTarget = strings.TrimSuffix(r.CNAME.String(), ".")
		}
	}

	// Follow CNAME if no direct A found
	if cnameTarget != "" {
		return resolveHostWithDepth(r, cnameTarget, depth+1)
	}

	return netip.Addr{}, fmt.Errorf("no A/AAAA record found for %s", host)
}
