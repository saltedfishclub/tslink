package netdiag

// This file collects every public address the machine appears to use, from as
// many different exits as possible.
//
// The methods are not redundant. STUN rides raw UDP, so it sees the address a
// peer would see and no HTTP proxy can touch it — that makes it the ground
// truth. The HTTP echo services are queried three ways: forced IPv4 with the
// proxy bypassed, forced IPv6 with the proxy bypassed, and through whatever
// proxy the environment advertises. When those answers disagree, traffic is
// being split across paths, and the address peers will actually connect back
// to is whichever path carries the tunnel — which is exactly the surprise this
// section exists to expose.

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// egTimeout bounds one HTTP echo query.
	egTimeout = 5 * time.Second
	// egMaxInflight bounds concurrent echo queries.
	egMaxInflight = 6
	// egMaxBody caps the echo response read. The services answer with a bare
	// IP; anything larger is a portal or an error page.
	egMaxBody = 4 << 10
)

func egLog(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(slog.String("from", "netdiag/egress"))
}

// egTarget is one HTTP echo service, queried over one specific path.
type egTarget struct {
	method   EgressMethod
	url      string
	region   Region
	network  string // "tcp4", "tcp6" or "" for unforced
	useProxy bool
}

// egTargets lists the echo services. All of them return a bare IP address in
// the body. The CN-hosted ones (ipw.cn) are kept because they stay reachable
// when the international ones are not, and their answer is what a domestic
// peer would see.
func egTargets() []egTarget {
	return []egTarget{
		// Forced IPv4, proxy explicitly bypassed.
		{method: MethodHTTPv4, url: "https://api.ipify.org", region: RegionIntl, network: "tcp4"},
		{method: MethodHTTPv4, url: "https://icanhazip.com", region: RegionIntl, network: "tcp4"},
		{method: MethodHTTPv4, url: "https://4.ipw.cn", region: RegionCN, network: "tcp4"},
		{method: MethodHTTPv4, url: "https://ipinfo.io/ip", region: RegionIntl, network: "tcp4"},

		// Forced IPv6, proxy explicitly bypassed.
		{method: MethodHTTPv6, url: "https://api6.ipify.org", region: RegionIntl, network: "tcp6"},
		{method: MethodHTTPv6, url: "https://6.ipw.cn", region: RegionCN, network: "tcp6"},

		// Unforced network, honouring HTTP(S)_PROXY.
		{method: MethodHTTPProxy, url: "https://api.ipify.org", region: RegionIntl, useProxy: true},
		{method: MethodHTTPProxy, url: "https://4.ipw.cn", region: RegionCN, useProxy: true},
	}
}

// ProbeEgress reports every public address this machine appears to use.
//
// stunResults are the already-collected STUN observations; STUN is not re-run
// here. Successful ones become [MethodSTUN] observations and serve as the
// proxy-immune reference the HTTP answers are compared against.
//
// The HTTP echo services are queried concurrently with a ~5s budget each.
// Geo and Countries are deliberately left empty; [AnnotateGeo] fills them so
// the caller can skip the third-party lookups entirely.
func ProbeEgress(ctx context.Context, stunResults []STUNResult, logger *slog.Logger) EgressReport {
	log := egLog(logger)

	var (
		mu   sync.Mutex
		obs  []EgressObservation
		wg   sync.WaitGroup
		sem  = make(chan struct{}, egMaxInflight)
		tgts = egTargets()
	)

	for _, r := range stunResults {
		if !r.OK {
			continue
		}
		ip := r.Mapped.Addr().Unmap().WithZone("")
		if !ip.IsValid() {
			continue
		}
		obs = append(obs, EgressObservation{
			Method: MethodSTUN,
			Source: r.Server,
			Region: r.Region,
			IP:     ip,
			RTT:    r.RTT,
		})
	}

	for _, t := range tgts {
		wg.Add(1)
		go func(t egTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			o := egQuery(ctx, t, log)
			mu.Lock()
			obs = append(obs, o)
			mu.Unlock()
		}(t)
	}
	wg.Wait()

	rep := EgressReport{Observations: obs}
	egSortObservations(rep.Observations)
	rep.UniqueIPs = egUniqueIPs(rep.Observations)
	rep.Divergent = egDivergent(rep.UniqueIPs)
	rep.DivergentSTUN = egDivergentSTUN(rep.Observations)
	egFinish(&rep)

	log.With(
		slog.Int("observations", len(rep.Observations)),
		slog.Int("unique_ips", len(rep.UniqueIPs)),
		slog.Bool("divergent", rep.Divergent),
		slog.String("status", rep.Status.String()),
	).Debug("finished egress probes")

	return rep
}

// egQuery asks one echo service for our address. Failures are recorded in the
// observation's Err field rather than returned, so a dead service still shows
// up as a row instead of vanishing.
func egQuery(ctx context.Context, t egTarget, log *slog.Logger) EgressObservation {
	o := EgressObservation{
		Method: t.method,
		Source: t.url,
		Region: t.region,
	}

	// Label the row by the path actually taken. Reporting a direct request as
	// MethodHTTPProxy would make the egress table claim a proxy was exercised
	// when none is configured.
	usedProxy := t.useProxy && diagProxyConfigured(t.url)
	if t.useProxy && !usedProxy {
		o.Source = t.url + "（未配置代理，实际直连）"
	}

	qctx, cancel := context.WithTimeout(ctx, egTimeout)
	defer cancel()

	client := newDiagClient(t.network, usedProxy, egTimeout)
	defer client.CloseIdleConnections()

	code, body, rtt, err := diagGet(qctx, client, t.url, egMaxBody, nil)
	o.RTT = rtt
	switch {
	case err != nil:
		o.Err = rchErrText(err)
	case code < 200 || code > 299:
		o.Err = fmt.Sprintf("unexpected status %d", code)
	default:
		text := strings.TrimSpace(string(body))
		ip, perr := netip.ParseAddr(text)
		if perr != nil {
			o.Err = fmt.Sprintf("unparseable response %q", egEllipsis(text, 48))
			break
		}
		o.IP = ip.Unmap().WithZone("")
	}

	log.With(
		slog.String("method", string(t.method)),
		slog.String("source", t.url),
		slog.String("ip", o.IP.String()),
		slog.Duration("rtt", o.RTT),
		slog.String("error", o.Err),
	).Debug("egress echo query done")

	return o
}

// egEllipsis truncates s for safe inclusion in an error string, so a hijacked
// response cannot dump a whole HTML page into the UI.
func egEllipsis(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// egSortObservations orders by method, then source, then address, so the table
// does not jitter between refreshes.
func egSortObservations(os []EgressObservation) {
	sort.Slice(os, func(i, j int) bool {
		x, y := os[i], os[j]
		if x.Method != y.Method {
			return x.Method < y.Method
		}
		if x.Source != y.Source {
			return x.Source < y.Source
		}
		return x.IP.Compare(y.IP) < 0
	})
}

// egUniqueIPs returns the deduplicated, sorted set of valid addresses.
func egUniqueIPs(os []EgressObservation) []netip.Addr {
	ips := make([]netip.Addr, 0, len(os))
	for _, o := range os {
		ips = append(ips, o.IP)
	}
	return egDedupAddrs(ips)
}

// egDedupAddrs drops invalid and repeated addresses and sorts the rest.
func egDedupAddrs(ips []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]struct{}, len(ips))
	var out []netip.Addr
	for _, ip := range ips {
		if !ip.IsValid() {
			continue
		}
		if _, dup := seen[ip]; dup {
			continue
		}
		seen[ip] = struct{}{}
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) < 0 })
	return out
}

// egSplitFamilies partitions addresses into IPv4 and IPv6 sets.
func egSplitFamilies(ips []netip.Addr) (v4, v6 []netip.Addr) {
	for _, ip := range ips {
		if ip.Is4() || ip.Is4In6() {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip)
		}
	}
	return v4, v6
}

// egDivergent reports whether the probes disagreed about our public address
// *within* an address family.
//
// A plain dual-stack host answers with one IPv4 and one IPv6 address, which is
// two distinct entries in UniqueIPs and entirely healthy. Treating that as
// disagreement would flag every dual-stack machine as proxied and bury the
// real signal — two different IPv4 addresses — in the noise.
func egDivergent(ips []netip.Addr) bool {
	v4, v6 := egSplitFamilies(ips)
	return len(v4) > 1 || len(v6) > 1
}

// egDivergentSTUN applies the same test to the STUN observations alone.
//
// Only these travel the UDP path Tailscale actually uses, so a split visible
// here is the one that costs you a direct connection. HTTP-only disagreement
// says something about the browser path, not the tunnel.
func egDivergentSTUN(obs []EgressObservation) bool {
	var ips []netip.Addr
	for _, o := range obs {
		if o.Method != MethodSTUN || o.Err != "" || !o.IP.IsValid() {
			continue
		}
		ips = append(ips, o.IP.Unmap())
	}
	return egDivergent(egDedupAddrs(ips))
}

// egFinish derives Status and the one-line Chinese Summary from the collected
// addresses. It is called again by [AnnotateGeo] once geolocation is known, so
// it must stay idempotent.
func egFinish(rep *EgressReport) {
	v4, v6 := egSplitFamilies(rep.UniqueIPs)

	switch {
	case len(rep.UniqueIPs) == 0:
		rep.Status = StatusFail
	case rep.DivergentSTUN:
		// The UDP egress itself varies, which is what actually costs a direct
		// connection — a stronger claim than "some probe disagreed".
		rep.Status = StatusFail
	case rep.Divergent:
		rep.Status = StatusWarn
	default:
		rep.Status = StatusOK
	}

	var b strings.Builder
	switch {
	case len(rep.UniqueIPs) == 0:
		b.WriteString("未能取得任何出口 IP：所有探测都失败了")

	case rep.Divergent:
		// Name the family that actually diverged, so a dual-stack host with a
		// split IPv4 path does not read as "everything is inconsistent".
		var parts []string
		if len(v4) > 1 {
			parts = append(parts, fmt.Sprintf("IPv4 有 %d 个（%s）", len(v4), egJoinAddrs(v4, 4)))
		}
		if len(v6) > 1 {
			parts = append(parts, fmt.Sprintf("IPv6 有 %d 个（%s）", len(v6), egJoinAddrs(v6, 4)))
		}
		if rep.DivergentSTUN {
			fmt.Fprintf(&b, "出口 IP 不一致：%s，STUN 探测本身就看到多个地址，代理、VPN 或多线接入正在拆分 UDP 流量，对端看到的地址取决于走哪条链路",
				strings.Join(parts, "；"))
		} else {
			// HTTP saw a split that STUN did not: the web path is proxied but
			// the UDP path Tailscale uses may well be intact.
			fmt.Fprintf(&b, "出口 IP 不一致：%s，仅 HTTP 探测存在差异，STUN（UDP）出口一致，多为浏览器代理或分流规则所致，通常不影响打洞",
				strings.Join(parts, "；"))
		}

	default:
		var parts []string
		if len(v4) == 1 {
			parts = append(parts, "IPv4 "+v4[0].String())
		}
		if len(v6) == 1 {
			parts = append(parts, "IPv6 "+v6[0].String())
		}
		fmt.Fprintf(&b, "出口 IP 唯一：%s", strings.Join(parts, "，"))
	}
	if len(rep.Countries) > 0 {
		fmt.Fprintf(&b, "，归属地 %s", strings.Join(rep.Countries, "、"))
	}
	rep.Summary = b.String()
}

// egJoinAddrs renders at most limit addresses for a summary line.
func egJoinAddrs(as []netip.Addr, limit int) string {
	parts := make([]string, 0, limit+1)
	for i, a := range as {
		if i >= limit {
			parts = append(parts, fmt.Sprintf("等 %d 个", len(as)))
			break
		}
		parts = append(parts, a.String())
	}
	return strings.Join(parts, "、")
}
