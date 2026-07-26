package netdiag

// This file resolves public IP addresses to a rough location and network
// operator.
//
// PRIVACY: every lookup here sends the user's own public IP address to a
// third-party API (ipinfo.io, ip-api.com, api.ip.sb). Those services see the
// address, the timestamp and our source IP — which for a direct query is that
// very same address. Nothing else is sent: no hostname, no tailnet identity,
// no credentials. Callers who are not comfortable with that must set
// [Options.SkipGeo], which exists precisely for this reason, and no request in
// this file will be made. The ipinfo token, when configured, is passed as a
// query parameter to that provider only and is never logged or stored in a
// report.
//
// Providers are tried in order and the first usable answer wins. The order is
// not arbitrary: ipinfo.io is the most accurate but rate-limits hard without a
// token, ip-api.com is HTTP-only on the free tier yet stays reachable from
// mainland China, and api.ip.sb is the last resort.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// geoTimeout bounds one provider query.
	geoTimeout = 4 * time.Second
	// geoMaxBody caps a provider response. Well-behaved answers are a few
	// hundred bytes; the cap guards against a hijacked or error page.
	geoMaxBody = 64 << 10
	// geoMaxInflight bounds concurrent lookups in [AnnotateGeo]. Kept low
	// because the free tiers of these APIs rate-limit per source IP.
	geoMaxInflight = 4
)

// geoCGNAT is RFC 6598 shared address space. Tailscale also allocates node
// addresses out of it, and either way no geolocation provider can say anything
// useful about such an address.
var geoCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// geoSkipErr is reported for addresses that are not globally routable.
const geoSkipErr = "私有地址，跳过查询"

func geoLog(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(slog.String("from", "netdiag/geo"))
}

// geoSkippable reports whether ip is not worth (or not safe to) look up:
// loopback, RFC1918/ULA private, link-local, CGNAT, multicast or unspecified.
func geoSkippable(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	if ip.Is4() && geoCGNAT.Contains(ip) {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// geoProvider is one geolocation backend.
type geoProvider struct {
	name string
	// fetch fills a GeoInfo from the provider, or returns an error so the next
	// provider is tried.
	fetch func(ctx context.Context, ip netip.Addr, token string) (GeoInfo, error)
}

// geoProviders returns the backends in the order they are tried.
func geoProviders() []geoProvider {
	return []geoProvider{
		{name: "ipinfo.io", fetch: geoFetchIPInfo},
		{name: "ip-api.com", fetch: geoFetchIPAPI},
		{name: "ip.sb", fetch: geoFetchIPSB},
	}
}

// LookupGeo resolves one address to a location and operator.
//
// Providers are tried in order until one answers; the returned GeoInfo names
// the provider that did in its Provider field. When all of them fail, Provider
// is empty and Err holds the last error. Addresses that are not globally
// routable are never sent anywhere: they come back immediately with Err set to
// "私有地址，跳过查询".
//
// token is an optional ipinfo.io API token; it raises that provider's rate
// limit and is never logged.
//
// Each provider gets its own ~4s budget, so the whole call is bounded even if
// every backend hangs. See the privacy note at the top of this file.
func LookupGeo(ctx context.Context, ip netip.Addr, token string, logger *slog.Logger) GeoInfo {
	log := geoLog(logger)
	ip = ip.Unmap().WithZone("")

	if geoSkippable(ip) {
		return GeoInfo{IP: ip, Err: geoSkipErr}
	}

	var lastErr string
	for _, p := range geoProviders() {
		if ctx.Err() != nil {
			return GeoInfo{IP: ip, Err: rchErrText(ctx.Err())}
		}
		pctx, cancel := context.WithTimeout(ctx, geoTimeout)
		info, err := p.fetch(pctx, ip, token)
		cancel()
		if err != nil {
			lastErr = fmt.Sprintf("%s: %s", p.name, rchErrText(err))
			log.With(
				slog.String("ip", ip.String()),
				slog.String("provider", p.name),
				slog.String("error", rchErrText(err)),
			).Debug("geo provider failed")
			continue
		}
		info.IP = ip
		info.Provider = p.name
		log.With(
			slog.String("ip", ip.String()),
			slog.String("provider", p.name),
			slog.String("country", info.Country),
			slog.String("asn", info.ASN),
		).Debug("resolved ip location")
		return info
	}

	if lastErr == "" {
		lastErr = "no geolocation provider answered"
	}
	return GeoInfo{IP: ip, Err: lastErr}
}

// AnnotateGeo fills rep.Geo and rep.Countries for every address in
// rep.UniqueIPs and recomputes rep.Summary. It is a no-op when the report has
// no addresses.
//
// Lookups run concurrently but at most [geoMaxInflight] at a time, since the
// free tiers rate-limit per source IP. Results are sorted by address and the
// country list is deduplicated, so repeated runs render identically.
//
// This function performs third-party network requests; see the privacy note at
// the top of this file and [Options.SkipGeo].
func AnnotateGeo(ctx context.Context, rep *EgressReport, token string, logger *slog.Logger) {
	if rep == nil || len(rep.UniqueIPs) == 0 {
		return
	}
	log := geoLog(logger)

	var (
		mu  sync.Mutex
		out = make([]GeoInfo, 0, len(rep.UniqueIPs))
		wg  sync.WaitGroup
		sem = make(chan struct{}, geoMaxInflight)
	)

	for _, ip := range rep.UniqueIPs {
		wg.Add(1)
		go func(ip netip.Addr) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				out = append(out, GeoInfo{IP: ip, Err: rchErrText(ctx.Err())})
				mu.Unlock()
				return
			}
			info := LookupGeo(ctx, ip, token, log)
			mu.Lock()
			out = append(out, info)
			mu.Unlock()
		}(ip)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].IP.Compare(out[j].IP) < 0 })
	rep.Geo = out
	rep.Countries = geoCountries(out)
	egFinish(rep)

	log.With(
		slog.Int("addrs", len(rep.Geo)),
		slog.String("countries", strings.Join(rep.Countries, ",")),
	).Debug("annotated egress addresses with geolocation")
}

// geoCountries returns the sorted, deduplicated set of countries seen,
// preferring the ISO code and falling back to the localised name when a
// provider only supplied that.
func geoCountries(gs []GeoInfo) []string {
	seen := make(map[string]struct{}, len(gs))
	var out []string
	for _, g := range gs {
		name := strings.TrimSpace(g.Country)
		if name == "" {
			name = strings.TrimSpace(g.CountryName)
		}
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Provider implementations
// ---------------------------------------------------------------------------

// geoGetJSON fetches url and decodes the body into v. The client uses the
// unforced network and honours the environment proxy: unlike the egress
// probes, we do not care which path the query takes, only that it succeeds.
func geoGetJSON(ctx context.Context, target string, v any) error {
	client := newDiagClient("", true, geoTimeout)
	defer client.CloseIdleConnections()

	hdr := http.Header{}
	hdr.Set("Accept", "application/json")

	code, body, _, err := diagGet(ctx, client, target, geoMaxBody, hdr)
	if err != nil {
		return err
	}
	if code < 200 || code > 299 {
		return fmt.Errorf("unexpected status %d", code)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("bad json: %w", err)
	}
	return nil
}

// geoIPInfoResp is the subset of ipinfo.io's answer we use.
type geoIPInfoResp struct {
	IP          string `json:"ip"`
	City        string `json:"city"`
	Region      string `json:"region"`
	Country     string `json:"country"`
	CountryName string `json:"country_name"` // only on paid plans
	Loc         string `json:"loc"`
	Org         string `json:"org"`
	Timezone    string `json:"timezone"`
	Bogon       bool   `json:"bogon"`
}

// geoFetchIPInfo queries ipinfo.io. The token, when non-empty, only raises the
// rate limit; it is appended as a query parameter and never logged.
func geoFetchIPInfo(ctx context.Context, ip netip.Addr, token string) (GeoInfo, error) {
	target := "https://ipinfo.io/" + url.PathEscape(ip.String()) + "/json"
	if token != "" {
		target += "?token=" + url.QueryEscape(token)
	}

	var r geoIPInfoResp
	if err := geoGetJSON(ctx, target, &r); err != nil {
		return GeoInfo{}, err
	}
	if r.Bogon {
		return GeoInfo{}, fmt.Errorf("provider reports bogon address")
	}
	if r.Country == "" && r.Org == "" && r.City == "" {
		return GeoInfo{}, fmt.Errorf("empty answer")
	}

	asn, org := geoSplitOrg(r.Org)
	return GeoInfo{
		Country:     strings.TrimSpace(r.Country),
		CountryName: strings.TrimSpace(r.CountryName),
		Region:      strings.TrimSpace(r.Region),
		City:        strings.TrimSpace(r.City),
		Org:         org,
		ASN:         asn,
		Loc:         strings.TrimSpace(r.Loc),
		Timezone:    strings.TrimSpace(r.Timezone),
	}, nil
}

// geoIPAPIResp is ip-api.com's answer for the field set we request.
type geoIPAPIResp struct {
	Status      string `json:"status"`
	Message     string `json:"message"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	RegionName  string `json:"regionName"`
	City        string `json:"city"`
	ISP         string `json:"isp"`
	Org         string `json:"org"`
	AS          string `json:"as"`
	Timezone    string `json:"timezone"`
}

// geoFetchIPAPI queries ip-api.com. The free tier is HTTP-only, which is also
// why it keeps working from mainland China where the HTTPS providers often do
// not. Answers are requested in Chinese to match the rest of the UI.
func geoFetchIPAPI(ctx context.Context, ip netip.Addr, _ string) (GeoInfo, error) {
	target := "http://ip-api.com/json/" + url.PathEscape(ip.String()) +
		"?lang=zh-CN&fields=status,message,country,countryCode,regionName,city,isp,org,as,timezone"

	var r geoIPAPIResp
	if err := geoGetJSON(ctx, target, &r); err != nil {
		return GeoInfo{}, err
	}
	if !strings.EqualFold(r.Status, "success") {
		msg := strings.TrimSpace(r.Message)
		if msg == "" {
			msg = r.Status
		}
		return GeoInfo{}, fmt.Errorf("query failed: %s", msg)
	}

	asn, asOrg := geoSplitOrg(r.AS)
	org := strings.TrimSpace(r.Org)
	if org == "" {
		org = strings.TrimSpace(r.ISP)
	}
	if org == "" {
		org = asOrg
	}
	return GeoInfo{
		Country:     strings.TrimSpace(r.CountryCode),
		CountryName: strings.TrimSpace(r.Country),
		Region:      strings.TrimSpace(r.RegionName),
		City:        strings.TrimSpace(r.City),
		Org:         org,
		ASN:         asn,
		Timezone:    strings.TrimSpace(r.Timezone),
	}, nil
}

// geoIPSBResp is api.ip.sb's answer. ASN comes back as a bare number, so it is
// decoded loosely and normalised by [geoASNText].
type geoIPSBResp struct {
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	Region      string `json:"region"`
	City        string `json:"city"`
	ISP         string `json:"isp"`
	ASN         any    `json:"asn"`
	ASNOrg      string `json:"asn_organization"`
	Timezone    string `json:"timezone"`
	Latitude    any    `json:"latitude"`
	Longitude   any    `json:"longitude"`
}

// geoFetchIPSB queries api.ip.sb, the last-resort provider.
func geoFetchIPSB(ctx context.Context, ip netip.Addr, _ string) (GeoInfo, error) {
	target := "https://api.ip.sb/geoip/" + url.PathEscape(ip.String())

	var r geoIPSBResp
	if err := geoGetJSON(ctx, target, &r); err != nil {
		return GeoInfo{}, err
	}
	if r.CountryCode == "" && r.Country == "" && r.ISP == "" {
		return GeoInfo{}, fmt.Errorf("empty answer")
	}

	org := strings.TrimSpace(r.ISP)
	if org == "" {
		org = strings.TrimSpace(r.ASNOrg)
	}
	return GeoInfo{
		Country:     strings.TrimSpace(r.CountryCode),
		CountryName: strings.TrimSpace(r.Country),
		Region:      strings.TrimSpace(r.Region),
		City:        strings.TrimSpace(r.City),
		Org:         org,
		ASN:         geoASNText(r.ASN),
		Loc:         geoLocText(r.Latitude, r.Longitude),
		Timezone:    strings.TrimSpace(r.Timezone),
	}, nil
}

// geoSplitOrg splits an "AS4134 Chinanet" style string into the ASN and the
// operator name. Either half may be missing, in which case the whole string is
// treated as the operator name.
func geoSplitOrg(s string) (asn, org string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	head, rest, _ := strings.Cut(s, " ")
	if geoLooksLikeASN(head) {
		return head, strings.TrimSpace(rest)
	}
	return "", s
}

// geoLooksLikeASN reports whether s is an "AS####" token.
func geoLooksLikeASN(s string) bool {
	if len(s) < 3 || !strings.EqualFold(s[:2], "AS") {
		return false
	}
	_, err := strconv.ParseUint(s[2:], 10, 32)
	return err == nil
}

// geoASNText normalises a JSON asn field (number or string) to "AS####".
func geoASNText(v any) string {
	var s string
	switch n := v.(type) {
	case nil:
		return ""
	case float64:
		if n <= 0 {
			return ""
		}
		s = strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		s = strings.TrimSpace(n)
	default:
		return ""
	}
	if s == "" || s == "0" {
		return ""
	}
	if geoLooksLikeASN(s) {
		return strings.ToUpper(s[:2]) + s[2:]
	}
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return ""
	}
	return "AS" + s
}

// geoLocText renders a latitude/longitude pair in ipinfo's "lat,lon" form so
// the Loc field means the same thing whichever provider answered.
func geoLocText(lat, lon any) string {
	f := func(v any) (string, bool) {
		switch n := v.(type) {
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64), true
		case string:
			s := strings.TrimSpace(n)
			return s, s != ""
		default:
			return "", false
		}
	}
	a, okA := f(lat)
	b, okB := f(lon)
	if !okA || !okB {
		return ""
	}
	return a + "," + b
}
