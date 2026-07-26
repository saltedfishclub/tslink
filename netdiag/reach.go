package netdiag

// This file answers one question: can traffic from this machine reach the
// wider internet, and does the answer change depending on how it leaves?
//
// Every probe is run twice-ish over deliberately different paths — forced
// IPv4, forced IPv6, and through whatever HTTP proxy the environment
// advertises. The divergence between those paths is the signal: a user running
// a proxy tool wants to see that the direct path is dead and the proxied one
// works (or the reverse), not have the two averaged into one green tick.
//
// The mainland-China targets are baselines. They separate "this machine has no
// internet at all" from "this machine has internet but cannot leave the
// country", which are two completely different things to fix.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// diagUserAgent identifies our probes to the servers we poke. Some captive
// portals and CDNs behave differently for an empty UA, and an honest one makes
// the traffic recognisable in a packet capture.
const diagUserAgent = "tslink-netdiag/1.0"

// diagMaxRedirects is the hard cap on redirects followed by any diagnostic
// client. A redirect chain is usually a portal bouncing us around; three hops
// is enough to land on it and few enough to stay inside the probe timeout.
const diagMaxRedirects = 3

const (
	// rchTimeout bounds a single reachability probe end to end.
	rchTimeout = 5 * time.Second
	// rchMaxInflight bounds concurrent reachability probes.
	rchMaxInflight = 6
	// rchMaxBody caps how much of a response body we read. The targets answer
	// 204 with no body at all; the cap only exists so a hijacking portal
	// serving a huge page cannot stall the probe.
	rchMaxBody = 64 << 10
)

func rchLog(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(slog.String("from", "netdiag/reach"))
}

// ---------------------------------------------------------------------------
// Shared HTTP plumbing (used by reach.go, egress.go and geo.go)
// ---------------------------------------------------------------------------

// uaTransport stamps [diagUserAgent] onto every request that does not already
// carry one. RoundTrippers must not mutate the request they are handed, so the
// request is cloned first.
type uaTransport struct {
	base http.RoundTripper
}

func (t uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") != "" {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("User-Agent", diagUserAgent)
	return t.base.RoundTrip(clone)
}

// newDiagClient builds a single-use HTTP client for one diagnostic probe.
//
// network forces the dial family: "tcp4", "tcp6", or "" to let the resolver
// and the kernel pick. Forcing the family is what makes an IPv4-only failure
// distinguishable from an IPv6-only one.
//
// useProxy selects [http.ProxyFromEnvironment] when true and no proxy at all
// when false. The false case is an explicit bypass, not a default: running the
// same target both ways is how proxy interference becomes visible.
//
// timeout bounds the whole request, including dial, TLS handshake and body
// read. Redirects are capped at [diagMaxRedirects] and the User-Agent is set
// to [diagUserAgent].
//
// The client keeps no idle connections; callers may still call
// CloseIdleConnections when they are done with it.
func newDiagClient(network string, useProxy bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}

	var proxy func(*http.Request) (*url.URL, error)
	if useProxy {
		proxy = http.ProxyFromEnvironment
	}

	tr := &http.Transport{
		Proxy: proxy,
		DialContext: func(ctx context.Context, defaultNetwork, addr string) (net.Conn, error) {
			if network != "" {
				defaultNetwork = network
			}
			return dialer.DialContext(ctx, defaultNetwork, addr)
		},
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}

	return &http.Client{
		Transport: uaTransport{base: tr},
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= diagMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", diagMaxRedirects)
			}
			return nil
		},
	}
}

// diagGet performs one GET and returns the status code, at most maxBody bytes
// of the body, and the time to a complete response. ctx must already carry the
// caller's deadline; nothing here blocks past it.
func diagGet(ctx context.Context, client *http.Client, target string, maxBody int64, header http.Header) (int, []byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, 0, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, time.Since(start), err
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	rtt := time.Since(start)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return resp.StatusCode, body, rtt, readErr
	}
	return resp.StatusCode, body, rtt, nil
}

// diagProxyConfigured reports whether the environment advertises a proxy for
// target. Only the boolean is ever surfaced: a proxy URL may embed credentials
// and must never reach a log line or a report field.
func diagProxyConfigured(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	req := &http.Request{URL: u, Header: http.Header{}}
	p, err := http.ProxyFromEnvironment(req)
	return err == nil && p != nil
}

// ---------------------------------------------------------------------------
// Overseas reachability
// ---------------------------------------------------------------------------

// rchTarget is one reachability probe definition.
type rchTarget struct {
	name     string
	url      string
	region   Region
	network  string // "tcp4", "tcp6" or "" for unforced
	viaProxy bool
	want     int // expected status code
}

// rchTargets is the probe list. Cloudflare's generate_204 is hit four ways
// because the paths fail independently.
//
// The direct and proxied Cloudflare probes deliberately use the same scheme:
// the point of running one target both ways is that the proxy setting is the
// only variable. Comparing plaintext-direct against TLS-proxied would let a
// middlebox that hijacks HTTP but passes HTTPS masquerade as "only the proxy
// works". The plaintext probe is kept separately, because that is exactly the
// signal a captive portal produces.
//
// The last two entries are mainland-China baselines.
func rchTargets() []rchTarget {
	return []rchTarget{
		{name: "Cloudflare 204 (IPv4)", url: "https://cp.cloudflare.com/generate_204", region: RegionIntl, network: "tcp4", want: http.StatusNoContent},
		{name: "Cloudflare 204 (IPv6)", url: "https://cp.cloudflare.com/generate_204", region: RegionIntl, network: "tcp6", want: http.StatusNoContent},
		{name: "Cloudflare 204 (代理)", url: "https://cp.cloudflare.com/generate_204", region: RegionIntl, viaProxy: true, want: http.StatusNoContent},
		{name: "Cloudflare 204 (明文/门户检测)", url: "http://cp.cloudflare.com/generate_204", region: RegionIntl, network: "tcp4", want: http.StatusNoContent},
		{name: "Gstatic 204", url: "http://www.gstatic.com/generate_204", region: RegionIntl, want: http.StatusNoContent},
		{name: "Google 204", url: "https://www.google.com/generate_204", region: RegionIntl, want: http.StatusNoContent},
		{name: "小米 204（国内基准）", url: "http://connect.rom.miui.com/generate_204", region: RegionCN, want: http.StatusNoContent},
		{name: "百度（国内基准）", url: "https://www.baidu.com", region: RegionCN, want: http.StatusOK},
	}
}

// ProbeOverseas checks whether traffic can leave for the wider internet.
//
// Every target is probed concurrently with its own ~5s budget, so the whole
// section finishes in about that time no matter how many probes hang. Probes
// against mainland-China targets act as a baseline: when they succeed and the
// international ones do not, the line is up but egress is filtered, which is a
// warning rather than a failure.
//
// A response that arrives with an unexpected status is recorded, not
// discarded — a captive portal or an injected block page is precisely what the
// user needs to see.
func ProbeOverseas(ctx context.Context, logger *slog.Logger) OverseasReport {
	log := rchLog(logger)
	targets := rchTargets()

	var (
		mu     sync.Mutex
		probes = make([]ReachProbe, 0, len(targets))
		wg     sync.WaitGroup
		sem    = make(chan struct{}, rchMaxInflight)
	)

	for _, t := range targets {
		wg.Add(1)
		go func(t rchTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				probes = append(probes, rchCancelled(t, ctx.Err()))
				mu.Unlock()
				return
			}
			p := rchProbeOne(ctx, t, log)
			mu.Lock()
			probes = append(probes, p)
			mu.Unlock()
		}(t)
	}
	wg.Wait()

	rchSortProbes(probes)
	rep := OverseasReport{Probes: probes}
	rchSummarize(&rep)

	log.With(
		slog.Int("probes", len(rep.Probes)),
		slog.String("status", rep.Status.String()),
	).Debug("finished overseas reachability probes")

	return rep
}

// rchCancelled builds the placeholder entry for a probe that never started
// because the run was cancelled. The row still renders, which is better than a
// silently shorter table.
func rchCancelled(t rchTarget, err error) ReachProbe {
	msg := "cancelled"
	if err != nil {
		msg = err.Error()
	}
	return ReachProbe{
		Name:     t.name,
		URL:      t.url,
		Region:   t.region,
		ViaProxy: t.viaProxy,
		Network:  t.network,
		Err:      msg,
	}
}

// rchProbeOne runs a single probe. It never returns an error: a failure is a
// datapoint, recorded in the probe's Err field.
func rchProbeOne(ctx context.Context, t rchTarget, log *slog.Logger) ReachProbe {
	// ViaProxy must record what happened, not what was intended. A client
	// built with http.ProxyFromEnvironment sends the request direct when no
	// proxy is configured, and counting that as proof the proxy path works is
	// how the summary ends up asserting "only the proxy link is usable" on a
	// machine with no proxy at all.
	usedProxy := t.viaProxy && diagProxyConfigured(t.url)

	p := ReachProbe{
		Name:     t.name,
		URL:      t.url,
		Region:   t.region,
		ViaProxy: usedProxy,
		Network:  t.network,
	}
	if t.viaProxy && !usedProxy {
		p.Name = t.name + "（环境未配置代理，实际直连）"
	}

	pctx, cancel := context.WithTimeout(ctx, rchTimeout)
	defer cancel()

	client := newDiagClient(t.network, usedProxy, rchTimeout)
	defer client.CloseIdleConnections()

	code, _, rtt, err := diagGet(pctx, client, t.url, rchMaxBody, nil)
	p.RTT = rtt
	p.StatusCode = code
	switch {
	case err != nil:
		p.Err = rchErrText(err)
	case code == t.want:
		p.OK = true
	default:
		// Reachable, but something answered on the target's behalf.
		p.Err = fmt.Sprintf("unexpected status %d (want %d), 可能存在门户劫持或内容注入", code, t.want)
	}

	log.With(
		slog.String("name", t.name),
		slog.String("network", rchNetworkText(t.network)),
		slog.Bool("via_proxy", t.viaProxy),
		slog.Int("status", p.StatusCode),
		slog.Duration("rtt", p.RTT),
		slog.Bool("ok", p.OK),
	).Debug("reachability probe done")

	return p
}

// rchErrText flattens a transport error into a short message. The URL is
// stripped because url.Error embeds the full target (and, for a proxied
// request, potentially proxy credentials) into its Error string.
func rchErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

func rchNetworkText(n string) string {
	if n == "" {
		return "auto"
	}
	return n
}

// rchSortProbes orders international probes before the CN baselines and is
// otherwise stable on URL/network/proxy, so consecutive refreshes render in
// exactly the same order.
func rchSortProbes(ps []ReachProbe) {
	rank := func(r Region) int {
		if r == RegionIntl {
			return 0
		}
		return 1
	}
	sort.Slice(ps, func(i, j int) bool {
		x, y := ps[i], ps[j]
		if rank(x.Region) != rank(y.Region) {
			return rank(x.Region) < rank(y.Region)
		}
		if x.URL != y.URL {
			return x.URL < y.URL
		}
		if x.Network != y.Network {
			return x.Network < y.Network
		}
		if x.ViaProxy != y.ViaProxy {
			return !x.ViaProxy
		}
		return x.Name < y.Name
	})
}

// rchSummarize derives Status and a one-line Chinese Summary.
//
// Any single successful international probe is enough for [StatusOK]: hosts
// without IPv6 are the norm, so a failed v6 probe alongside a working v4 one
// must not drag the verdict down. Only the CN baselines succeeding means the
// local network is fine but the wider internet is not reachable
// ([StatusWarn]); nothing succeeding at all is [StatusFail].
func rchSummarize(rep *OverseasReport) {
	var (
		intlOK, intlTotal int
		cnOK, cnTotal     int
		proxyOK           bool
		directIntlOK      bool
		v6OK              bool
		hijacked          int
	)
	for _, p := range rep.Probes {
		if p.Region == RegionIntl {
			intlTotal++
			if p.OK {
				intlOK++
				if p.ViaProxy {
					proxyOK = true
				} else {
					directIntlOK = true
				}
				if p.Network == "tcp6" {
					v6OK = true
				}
			}
		} else {
			cnTotal++
			if p.OK {
				cnOK++
			}
		}
		if !p.OK && p.StatusCode > 0 {
			hijacked++
		}
	}

	var b strings.Builder
	switch {
	case intlOK > 0:
		rep.Status = StatusOK
		fmt.Fprintf(&b, "境外可达（%d/%d 个境外目标成功）", intlOK, intlTotal)
		switch {
		case proxyOK && !directIntlOK:
			b.WriteString("，仅代理链路可用，直连被阻断")
		case directIntlOK && !proxyOK && diagProxyConfigured("https://cp.cloudflare.com/generate_204"):
			b.WriteString("，直连可用但代理链路失败")
		}
		if !v6OK {
			b.WriteString("，IPv6 不可用（不影响判定）")
		}
	case cnOK > 0:
		rep.Status = StatusWarn
		fmt.Fprintf(&b, "境外不可达（0/%d），但本地网络正常：国内基准 %d/%d 通过，问题在跨境链路而非本机网络", intlTotal, cnOK, cnTotal)
	default:
		rep.Status = StatusFail
		fmt.Fprintf(&b, "境内外目标均无法访问（0/%d），本机可能完全没有网络", intlTotal+cnTotal)
	}
	if hijacked > 0 {
		fmt.Fprintf(&b, "；%d 个目标返回了非预期状态码，疑似门户或注入", hijacked)
	}
	rep.Summary = b.String()
}
