package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dohContentType is the RFC 8484 media type for binary DNS messages over HTTPS.
const dohContentType = "application/dns-message"

// maxDoHResponse bounds how much of a DoH response body we read, guarding against
// a hostile or misbehaving endpoint streaming an unbounded body.
const maxDoHResponse = 64 << 10 // 64 KiB

// Configured DNS-over-HTTPS endpoints, set once at startup from the config file
// (see SetDoHServers). Mirrors the magicDNSSuffix package-global pattern in
// dns.go so the resolver need not thread config through every call.
var (
	dohMu      sync.RWMutex
	dohServers []string
	dohClient  = &http.Client{Timeout: 10 * time.Second}
)

// SetDoHServers records the DNS-over-HTTPS fallback endpoints.
func SetDoHServers(servers []string) {
	dohMu.Lock()
	defer dohMu.Unlock()
	dohServers = append([]string(nil), servers...)
}

// dohEnabled reports whether any DoH fallback endpoint is configured.
func dohEnabled() bool {
	dohMu.RLock()
	defer dohMu.RUnlock()
	return len(dohServers) > 0
}

// getDoHServers returns a copy of the configured DoH endpoints.
func getDoHServers() []string {
	dohMu.RLock()
	defer dohMu.RUnlock()
	return append([]string(nil), dohServers...)
}

// resolveHostViaDoH resolves host through the configured DoH endpoints.
func resolveHostViaDoH(ctx context.Context, host string) (netip.Addr, error) {
	return resolveViaDoHServers(ctx, getDoHServers(), host)
}

// resolveViaDoHServers tries each server in order, returning the first address
// that resolves. It takes the server list explicitly so it can be exercised in
// tests without touching package globals.
func resolveViaDoHServers(ctx context.Context, servers []string, host string) (netip.Addr, error) {
	if len(servers) == 0 {
		return netip.Addr{}, errors.New("no DoH servers configured")
	}
	var lastErr error
	for _, server := range servers {
		ip, err := resolveHostChase(ctx, host, 0, dohExchange(server))
		if err != nil {
			lastErr = fmt.Errorf("doh %s: %w", server, err)
			continue
		}
		return ip, nil
	}
	return netip.Addr{}, lastErr
}

// dohExchange returns a dnsExchange that resolves a single question against a
// single DoH endpoint using the RFC 8484 binary wire format over HTTPS POST.
func dohExchange(server string) dnsExchange {
	return func(ctx context.Context, name dnsmessage.Name, qType dnsmessage.Type) (netip.Addr, string, error) {
		query, err := buildDNSQuery(name, qType)
		if err != nil {
			return netip.Addr{}, "", err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, bytes.NewReader(query))
		if err != nil {
			return netip.Addr{}, "", fmt.Errorf("build DoH request: %w", err)
		}
		req.Header.Set("Content-Type", dohContentType)
		req.Header.Set("Accept", dohContentType)

		resp, err := dohClient.Do(req)
		if err != nil {
			return netip.Addr{}, "", fmt.Errorf("DoH request to %s failed: %w", server, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return netip.Addr{}, "", fmt.Errorf("DoH request to %s: unexpected status %d", server, resp.StatusCode)
		}

		respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxDoHResponse))
		if err != nil {
			return netip.Addr{}, "", fmt.Errorf("read DoH response from %s: %w", server, err)
		}
		return parseDNSAnswer(respBytes)
	}
}
