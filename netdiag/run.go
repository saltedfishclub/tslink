package netdiag

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run executes the full diagnostic suite and returns a populated report.
//
// The phases are deliberately not all parallel. Interface enumeration, port
// mapping, overseas reachability and tailscale's own netcheck are independent
// and run together. The STUN-derived phases are staged: a burst of binding
// requests first, then NAT classification on its own socket, then egress
// discovery reusing the results we already have. Running all of them at once
// would triple the load on a handful of public STUN servers and make the
// mapping tests race each other's sockets.
//
// Run always returns a report, even when everything failed; partial results
// are the normal case on a broken network and are exactly what the user needs
// to see.
func Run(ctx context.Context, opt Options) *Report {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logger := opt.logger()
	servers := opt.STUNServers
	if len(servers) == 0 {
		servers = DefaultSTUNServers()
	}

	rep := &Report{StartedAt: time.Now()}
	total := len(Steps)
	if opt.Tailscale == nil {
		total--
	}
	if opt.SkipGeo {
		total--
	}

	// Phases run concurrently, so each one's index has to be captured when it
	// starts. Re-reading the shared counter on completion would make the
	// "step N of M" label jump around and even count backwards.
	type stepStart struct {
		idx int
		at  time.Time
	}
	var (
		mu      sync.Mutex
		index   int
		started = make(map[string]stepStart)
	)
	begin := func(key string) {
		mu.Lock()
		index++
		s := stepStart{idx: index, at: time.Now()}
		started[key] = s
		mu.Unlock()
		opt.progress(Progress{Key: key, Title: stepTitle(key), Index: s.idx, Total: total})
	}
	finish := func(key, errText string) {
		mu.Lock()
		s := started[key]
		mu.Unlock()
		opt.progress(Progress{
			Key: key, Title: stepTitle(key), Index: s.idx, Total: total,
			Done: true, Err: errText, Elapsed: time.Since(s.at),
		})
	}

	var wg sync.WaitGroup

	// --- independent probes -------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		begin("iface")
		r := EnumerateInterfaces(ctx, logger)
		mu.Lock()
		rep.Interfaces = r
		mu.Unlock()
		finish("iface", r.Err)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		begin("portmap")
		r := ProbePortMapping(ctx, logger)
		mu.Lock()
		rep.PortMap = r
		mu.Unlock()
		finish("portmap", "")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		begin("overseas")
		r := ProbeOverseas(ctx, logger)
		mu.Lock()
		rep.Overseas = r
		mu.Unlock()
		finish("overseas", "")
	}()

	if opt.Tailscale != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			begin("tailscale")
			tsCtx, tsCancel := context.WithTimeout(ctx, 20*time.Second)
			defer tsCancel()
			r, err := opt.Tailscale.Netcheck(tsCtx)
			errText := ""
			mu.Lock()
			switch {
			case r != nil:
				rep.Tailscale = *r
			case err != nil:
				rep.Tailscale = TailscaleReport{Status: StatusSkipped, Err: err.Error()}
			default:
				// Defensive: a source that returns neither a report nor an
				// error still failed to measure, and Err is what marks the
				// difference between that and "no source was configured".
				rep.Tailscale = TailscaleReport{
					Status: StatusSkipped,
					Err:    "netcheck returned no report",
				}
			}
			if err != nil {
				errText = err.Error()
			}
			mu.Unlock()
			finish("tailscale", errText)
		}()
	} else {
		rep.Tailscale = TailscaleReport{
			Status:  StatusSkipped,
			Summary: "Tailscale 未运行，跳过内部状态检查",
		}
	}

	// --- STUN-derived chain -------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()

		begin("udp")
		var (
			stunResults []STUNResult
			udpReport   UDPReport
			inner       sync.WaitGroup
		)
		inner.Add(2)
		go func() {
			defer inner.Done()
			stunResults = ProbeSTUN(ctx, servers, logger)
		}()
		go func() {
			defer inner.Done()
			udpReport = ProbeUDP(ctx, servers, logger)
		}()
		inner.Wait()

		mu.Lock()
		rep.UDP = udpReport
		mu.Unlock()
		finish("udp", "")

		begin("nat")
		nat := ClassifyNAT(ctx, servers, logger)
		mu.Lock()
		rep.NAT = nat
		mu.Unlock()
		finish("nat", "")

		begin("egress")
		egress := ProbeEgress(ctx, stunResults, logger)
		mu.Lock()
		rep.Egress = egress
		mu.Unlock()
		finish("egress", "")

		if opt.SkipGeo {
			mu.Lock()
			rep.Egress.Summary = strings.TrimSpace(rep.Egress.Summary + " 已跳过归属地查询。")
			mu.Unlock()
			return
		}

		begin("geo")
		mu.Lock()
		target := rep.Egress
		mu.Unlock()
		AnnotateGeo(ctx, &target, opt.IPInfoToken, logger)
		mu.Lock()
		rep.Egress = target
		mu.Unlock()
		finish("geo", "")
	}()

	wg.Wait()

	rep.FinishedAt = time.Now()
	rep.Duration = rep.FinishedAt.Sub(rep.StartedAt)
	rep.Status = worstStatus(
		rep.Interfaces.Status,
		rep.UDP.Status,
		rep.NAT.Status,
		rep.PortMap.Status,
		rep.Overseas.Status,
		rep.Egress.Status,
		rep.Tailscale.Status,
	)
	rep.Headline, rep.HeadlineStatus = headline(rep)
	logger.Info("diagnostics finished",
		"took", rep.Duration.Round(time.Millisecond),
		"status", rep.Status.String(),
		"headline", rep.Headline,
	)
	return rep
}

func stepTitle(key string) string {
	for _, s := range Steps {
		if s.Key == key {
			return s.Title
		}
	}
	return key
}

// headline picks the single most consequential finding, together with that
// sentence's own severity. The ordering is by how badly each condition breaks
// the thing this app exists to do — carry game traffic between peers — not by
// section order.
//
// The severity is returned separately because Report.Status is the worst of
// every section: an unrelated port-mapping failure would otherwise render a
// "may be affecting" headline in the same red as "is affecting", which is
// exactly the overstatement this split exists to prevent.
func headline(r *Report) (string, Status) {
	switch {
	case r.NAT.Type == NATUDPBlocked:
		return "UDP 被完全阻断，无法建立直连，所有流量都会走 DERP 中继", StatusFail
	case !r.UDP.V4OK && !r.UDP.V6OK:
		return "UDP 探测全部失败，请检查防火墙或网络策略", StatusFail
	case r.NAT.Type == NATSymmetric:
		return "对称型 NAT：与同样受限的对端难以打洞，连接多半会退回中继", StatusFail
	case r.Overseas.Status == StatusFail:
		return "无法访问任何外部网络", StatusFail
	case r.Overseas.Status == StatusWarn:
		return "境外网络不可达，Tailscale 控制面与 DERP 可能受影响", StatusWarn
	case r.Egress.DivergentSTUN:
		// STUN itself saw several egress addresses: the UDP path Tailscale uses
		// really does vary per flow.
		return "STUN 检测到多个出口 IP，代理或分流工具正在影响连接", StatusFail
	case r.Egress.Divergent:
		// Only the web path disagreed; UDP may well be intact.
		return "仅 HTTP 探测到多个出口 IP，代理或分流工具可能影响连接", StatusWarn
	case r.PortMap.Status == StatusWarn && r.NAT.Type == NATPortRestrict:
		return "路由器未提供端口映射，NAT 为端口限制型，打洞成功率一般", StatusWarn
	}

	// No single finding is worth leading with, so what is left is whether the
	// run itself worked.
	//
	// This is checked before the all-clear below, not after. Section statuses
	// can all read OK while a section quietly failed to measure anything — a
	// NAT whose filtering behaviour is unknown is the common one — and
	// answering that with "具备直连条件" asserts exactly the property that was
	// never determined.
	if missing := indeterminate(r); len(missing) > 0 {
		return "诊断完成；未能得出结论的项目：" + strings.Join(missing, "、"), StatusWarn
	}
	if r.Status == StatusOK {
		return "网络状况良好，具备直连条件", StatusOK
	}
	return "诊断完成，各项检测均已得出结论", StatusOK
}

// indeterminate names the sections that could not produce an answer, as opposed
// to producing an unwelcome one.
//
// That distinction is what the closing headline is for. "The router does not
// speak UPnP" and "this NAT is port-restricted" are answers: the diagnostic
// worked, and the user now knows something true about their network. Reporting
// those as "存在若干需要注意的项目" in alarm colours said the run had gone wrong
// when it had gone exactly right, and — because Report.Status is the worst of
// every section — a single unremarkable finding was enough to trigger it.
//
// Only a section that failed to measure belongs here.
func indeterminate(r *Report) []string {
	var out []string
	if r.Interfaces.Err != "" {
		out = append(out, "本机地址")
	}
	if r.UDP.Status == StatusSkipped || len(r.UDP.Probes) == 0 {
		out = append(out, "UDP 连通性")
	}
	// A NAT is undetermined when the classifier could not name it. Every other
	// type, symmetric and UDP-blocked included, is a real measurement.
	if r.NAT.Type == NATUnknown || r.NAT.Mapping == BehaviorUnknown || r.NAT.Filtering == BehaviorUnknown {
		out = append(out, "NAT 类型")
	}
	// "Router answered and does not support it" is an answer; "no gateway to
	// ask" is not.
	if r.PortMap.Status == StatusSkipped || !r.PortMap.Gateway.IsValid() {
		out = append(out, "端口映射")
	}
	if r.Overseas.Status == StatusSkipped || len(r.Overseas.Probes) == 0 {
		out = append(out, "境外连通性")
	}
	if len(r.Egress.UniqueIPs) == 0 {
		out = append(out, "出口 IP")
	}
	// No tailscale source at all is the caller opting out — the same kind of
	// choice as SkipGeo, not a measurement that failed. Only a netcheck that
	// was actually attempted and came back empty belongs here, and Err is what
	// distinguishes the two.
	if r.Tailscale.Status == StatusSkipped && r.Tailscale.Err != "" {
		out = append(out, "Tailscale 内部状态")
	}
	return out
}

// ---------------------------------------------------------------------------
// Text report
// ---------------------------------------------------------------------------

// Text renders the report as a plain-text block suitable for pasting into an
// issue or a paste service. It contains no credentials, but it does contain
// the machine's public and private addresses, which is unavoidable for a
// network diagnostic and worth telling the user before they share it.
func (r *Report) Text() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("=== tslink 网络诊断报告 ===\n")
	w("时间: %s\n", r.StartedAt.Format(time.RFC3339))
	w("耗时: %s\n", r.Duration.Round(time.Millisecond))
	// The severity printed here is the headline's own, so the tag and the
	// sentence beside it cannot contradict each other. Report.Status is the
	// worst of every section and is still what each section header below shows.
	w("总评: [%s] %s\n\n", strings.ToUpper(r.HeadlineStatus.String()), r.Headline)

	// --- interfaces --------------------------------------------------------
	w("--- 本机地址 [%s] ---\n", r.Interfaces.Status)
	if r.Interfaces.Summary != "" {
		w("%s\n", r.Interfaces.Summary)
	}
	if r.Interfaces.DefaultV4Src.IsValid() {
		w("默认 IPv4 源: %s\n", r.Interfaces.DefaultV4Src)
	}
	if r.Interfaces.DefaultV6Src.IsValid() {
		w("默认 IPv6 源: %s\n", r.Interfaces.DefaultV6Src)
	}
	for _, a := range r.Interfaces.Addrs {
		flag := ""
		if a.IsDefaultSrc {
			flag = " *默认出口"
		}
		w("  %-14s %-40s %-10s%s\n", a.Iface, a.Addr.String(), a.Kind, flag)
	}
	if r.Interfaces.Err != "" {
		w("错误: %s\n", r.Interfaces.Err)
	}
	b.WriteByte('\n')

	// --- udp ---------------------------------------------------------------
	w("--- UDP 连通性 [%s] ---\n", r.UDP.Status)
	if r.UDP.Summary != "" {
		w("%s\n", r.UDP.Summary)
	}
	w("IPv4: %v  IPv6: %v  国内 %d/%d  国外 %d/%d\n",
		r.UDP.V4OK, r.UDP.V6OK,
		r.UDP.CNReachable, r.UDP.CNTotal, r.UDP.IntlReachabl, r.UDP.IntlTotal)
	if len(r.UDP.BlockedPorts) > 0 {
		w("疑似被封端口: %v\n", r.UDP.BlockedPorts)
	}
	for _, p := range r.UDP.Probes {
		status := "FAIL"
		detail := p.Err
		if p.OK {
			status = "OK"
			detail = p.Mapped.String() + " " + p.RTT.Round(time.Millisecond).String()
		}
		// Name the server, then the address actually probed — a shared bundle
		// has to be readable without the reader resolving IPs by hand.
		target := p.Host
		if target == "" {
			target = p.Target
		} else if p.Target != "" && p.Target != p.Host {
			target += " (" + p.Target + ")"
		}
		w("  %-4s %-46s %-5s %s\n", status, target, p.Region, detail)
	}
	b.WriteByte('\n')

	// --- nat ---------------------------------------------------------------
	w("--- NAT 类型 [%s] ---\n", r.NAT.Status)
	w("类型: %s\n", r.NAT.Type)
	w("映射行为: %s\n", r.NAT.Mapping)
	w("过滤行为: %s\n", r.NAT.Filtering)
	w("发夹回环: %s\n", triState(r.NAT.Hairpin))
	w("端口保持: %s\n", triState(r.NAT.PortPreserving))
	if len(r.NAT.MappedAddrs) > 0 {
		addrs := make([]string, 0, len(r.NAT.MappedAddrs))
		for _, a := range r.NAT.MappedAddrs {
			addrs = append(addrs, a.String())
		}
		w("观测到的映射地址: %s\n", strings.Join(addrs, ", "))
	}
	if r.NAT.Summary != "" {
		w("%s\n", r.NAT.Summary)
	}
	for _, n := range r.NAT.Notes {
		w("注: %s\n", n)
	}
	b.WriteByte('\n')

	// --- port mapping ------------------------------------------------------
	w("--- 端口映射 [%s] ---\n", r.PortMap.Status)
	if r.PortMap.Gateway.IsValid() {
		w("网关: %s\n", r.PortMap.Gateway)
	}
	writeService(&b, "UPnP IGD", r.PortMap.UPnP)
	writeService(&b, "NAT-PMP ", r.PortMap.NATPMP)
	writeService(&b, "PCP     ", r.PortMap.PCP)
	if r.PortMap.Summary != "" {
		w("%s\n", r.PortMap.Summary)
	}
	b.WriteByte('\n')

	// --- overseas ----------------------------------------------------------
	w("--- 境外连通性 [%s] ---\n", r.Overseas.Status)
	if r.Overseas.Summary != "" {
		w("%s\n", r.Overseas.Summary)
	}
	for _, p := range r.Overseas.Probes {
		status := "FAIL"
		if p.OK {
			status = "OK"
		}
		via := "direct"
		if p.ViaProxy {
			via = "proxy"
		}
		net := p.Network
		if net == "" {
			net = "auto"
		}
		detail := p.RTT.Round(time.Millisecond).String()
		if p.Err != "" {
			detail = p.Err
		}
		w("  %-4s %-3d %-6s %-5s %-46s %s\n", status, p.StatusCode, via, net, p.URL, detail)
	}
	b.WriteByte('\n')

	// --- egress ------------------------------------------------------------
	w("--- 出口 IP [%s] ---\n", r.Egress.Status)
	if r.Egress.Summary != "" {
		w("%s\n", r.Egress.Summary)
	}
	if r.Egress.DivergentSTUN {
		w("!! STUN(UDP) 本身看到多个公网 IP，直连打洞会受影响\n")
	} else if r.Egress.Divergent {
		w("!! 仅 HTTP 探测得到了不同的公网 IP，STUN(UDP) 出口一致，通常不影响打洞\n")
	}
	for _, o := range r.Egress.Observations {
		val := o.IP.String()
		if !o.IP.IsValid() {
			val = "(" + o.Err + ")"
		}
		w("  %-11s %-5s %-40s %s\n", o.Method, o.Region, o.Source, val)
	}
	for _, g := range r.Egress.Geo {
		if g.Err != "" && g.Provider == "" {
			w("  %-40s %s\n", g.IP.String(), g.Err)
			continue
		}
		parts := []string{}
		for _, p := range []string{g.CountryName, g.Country, g.Region, g.City} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		w("  %-40s %s | %s %s (via %s)\n",
			g.IP.String(), strings.Join(parts, " "), g.ASN, g.Org, g.Provider)
	}
	b.WriteByte('\n')

	// --- tailscale ---------------------------------------------------------
	w("--- Tailscale 内部状态 [%s] ---\n", r.Tailscale.Status)
	if r.Tailscale.Summary != "" {
		w("%s\n", r.Tailscale.Summary)
	}
	if r.Tailscale.Available {
		w("UDP: %v  IPv4: %v  IPv6: %v  ICMPv4: %v\n",
			r.Tailscale.UDP, r.Tailscale.IPv4, r.Tailscale.IPv6, r.Tailscale.ICMPv4)
		w("门户劫持: %s\n", triState(r.Tailscale.CaptivePortal))
		if r.Tailscale.GlobalV4 != "" {
			w("GlobalV4: %s\n", r.Tailscale.GlobalV4)
		}
		if r.Tailscale.GlobalV6 != "" {
			w("GlobalV6: %s\n", r.Tailscale.GlobalV6)
		}
		w("首选 DERP: %s\n", r.Tailscale.PreferredDERP)
		derp := append([]DERPLatency(nil), r.Tailscale.DERP...)
		sort.Slice(derp, func(i, j int) bool { return derp[i].Latency < derp[j].Latency })
		for i, d := range derp {
			if i >= 8 {
				break
			}
			mark := " "
			if d.Preferred {
				mark = "*"
			}
			w("  %s %-6s %-24s %s\n", mark, d.RegionCode, d.Name,
				d.Latency.Round(time.Millisecond))
		}
	}
	if r.Tailscale.Err != "" {
		w("错误: %s\n", r.Tailscale.Err)
	}

	return b.String()
}

func writeService(b *strings.Builder, name string, s ServiceProbe) {
	status := "不支持"
	if s.Available {
		status = "支持"
	}
	line := "  " + name + ": " + status
	if s.ExternalIP.IsValid() {
		line += "  外部地址 " + s.ExternalIP.String()
	}
	if s.Detail != "" {
		line += "  " + s.Detail
	}
	if s.Err != "" {
		line += "  (" + s.Err + ")"
	}
	b.WriteString(line + "\n")
}

func triState(v *bool) string {
	if v == nil {
		return "未知"
	}
	if *v {
		return "是"
	}
	return "否"
}
