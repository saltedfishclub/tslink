package netdiag

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// tunables
// ---------------------------------------------------------------------------

const (
	// pmPort is shared by NAT-PMP (RFC 6886) and PCP (RFC 6887).
	pmPort = 5351
	// pmSSDPTarget is the SSDP multicast rendezvous address.
	pmSSDPTarget = "239.255.255.250:1900"

	// pmBudget bounds the whole [ProbePortMapping] call.
	pmBudget = 6 * time.Second
	// pmProtoBudget bounds one protocol probe across every gateway candidate.
	pmProtoBudget = 4500 * time.Millisecond
	// pmUnicastTimeout is the per-attempt wait for a NAT-PMP/PCP reply.
	pmUnicastTimeout = 750 * time.Millisecond
	// pmRetries is the number of *extra* attempts after the first one.
	pmRetries = 2
	// pmSSDPMX is the MX header we advertise: devices answer after a random
	// delay in [0, MX] seconds.
	pmSSDPMX = 2
	// pmSSDPCollect is how long we listen for SSDP replies, measured from the
	// moment the M-SEARCH goes out. It must exceed pmSSDPMX, otherwise devices
	// that happen to draw a delay near the top of the range are cut off and
	// the IGD is intermittently reported as absent.
	pmSSDPCollect = 3 * time.Second
	// pmHTTPTimeout bounds one description fetch or SOAP call.
	pmHTTPTimeout = 2 * time.Second
	// pmMaxDescBody caps a device description body.
	pmMaxDescBody = 256 << 10
	// pmMaxGateways caps how many gateway candidates we are willing to poke.
	pmMaxGateways = 4
	// pmMaxSSDPSockets bounds the in-flight SSDP goroutines.
	pmMaxSSDPSockets = 8
	// pmMaxLocations caps how many distinct SSDP LOCATIONs we fetch.
	pmMaxLocations = 4
)

// pmWANServices are the IGD service types that expose GetExternalIPAddress,
// in preference order.
var pmWANServices = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// pmLog returns a logger tagged for this subsystem, tolerating a nil logger.
func pmLog(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(slog.String("from", "netdiag/portmap"))
}

// pmDeadline returns the shorter of ctx's deadline and now+d.
func pmDeadline(ctx context.Context, d time.Duration) time.Time {
	t := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Before(t) {
		return dl
	}
	return t
}

// ---------------------------------------------------------------------------
// gateway discovery
// ---------------------------------------------------------------------------

// DiscoverGateways returns candidate default-gateway addresses, best first.
//
// On Linux the kernel routing tables are parsed directly; everywhere else (and
// as a fallback when the tables yield nothing) the conventional first and last
// host addresses of every private IPv4 prefix on an up interface are offered.
// The result is deduplicated, ordered deterministically and capped at four
// entries. It never shells out and never blocks on the network.
func DiscoverGateways(ctx context.Context, logger *slog.Logger) []netip.Addr {
	gws, _ := pmGateways(ctx, logger)
	return gws
}

// pmGateways is [DiscoverGateways] plus the interface enumeration error, which
// [ProbePortMapping] needs to tell "no router" apart from "no network stack".
func pmGateways(ctx context.Context, logger *slog.Logger) ([]netip.Addr, error) {
	log := pmLog(logger)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var out []netip.Addr
	out = append(out, pmProcRoutes(log)...)

	ifaces, ifErr := net.Interfaces()
	if ifErr != nil {
		log.Debug("cannot enumerate interfaces", slog.String("error", ifErr.Error()))
	} else {
		out = append(out, pmGuessedGateways(ifaces)...)
	}

	out = pmDedupAddrs(out)
	if len(out) > pmMaxGateways {
		out = out[:pmMaxGateways]
	}
	if len(out) == 0 && ifErr != nil {
		return nil, ifErr
	}
	log.Debug("gateway candidates", slog.Int("count", len(out)), slog.String("addrs", pmJoinAddrs(out)))
	return out, nil
}

// pmProcRoutes reads the Linux routing tables. It returns nil on any other
// platform, or when the files are unreadable.
func pmProcRoutes(log *slog.Logger) []netip.Addr {
	var out []netip.Addr
	out = append(out, pmProcRoute4(log)...)
	out = append(out, pmProcRoute6(log)...)
	return out
}

// pmRouteCandidate is a parsed routing-table row, kept so rows can be ordered
// by metric before their gateways are handed out.
type pmRouteCandidate struct {
	addr   netip.Addr
	metric int
	iface  string
}

// pmSortRoutes orders routes by metric, then interface, then address, so the
// candidate list does not jitter between refreshes.
//
// Link-local IPv6 next hops get their interface zone reattached. A Linux IPv6
// default route almost always points at an fe80:: address, and dialling one
// without a zone fails outright — so without this the PCP-over-IPv6 probe
// never reaches the router, and the unusable candidates still consume slots
// against pmMaxGateways, crowding out the IPv4 guesses.
func pmSortRoutes(rs []pmRouteCandidate) []netip.Addr {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].metric != rs[j].metric {
			return rs[i].metric < rs[j].metric
		}
		if rs[i].iface != rs[j].iface {
			return rs[i].iface < rs[j].iface
		}
		return rs[i].addr.Compare(rs[j].addr) < 0
	})
	out := make([]netip.Addr, 0, len(rs))
	for _, r := range rs {
		addr := r.addr
		if addr.Is6() && addr.IsLinkLocalUnicast() {
			if r.iface == "" {
				continue // unusable without a zone
			}
			addr = addr.WithZone(r.iface)
		}
		out = append(out, addr)
	}
	return out
}

const (
	pmRTFUp      = 0x0001
	pmRTFGateway = 0x0002
)

// pmProcRoute4 parses /proc/net/route. Every numeric column is hex; the
// address columns are little-endian, so 0102A8C0 is 192.168.2.1. The default
// route is the row with a zero destination and the RTF_GATEWAY flag.
func pmProcRoute4(log *slog.Logger) []netip.Addr {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		log.Debug("no /proc/net/route", slog.String("error", err.Error()))
		return nil
	}
	var rows []pmRouteCandidate
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 { // header
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		dest, err1 := strconv.ParseUint(f[1], 16, 32)
		gw, err2 := strconv.ParseUint(f[2], 16, 32)
		flags, err3 := strconv.ParseUint(f[3], 16, 32)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if dest != 0 || flags&pmRTFGateway == 0 || flags&pmRTFUp == 0 || gw == 0 {
			continue
		}
		metric := 0
		if len(f) >= 7 {
			if m, err := strconv.Atoi(f[6]); err == nil {
				metric = m
			}
		}
		v := uint32(gw)
		addr := netip.AddrFrom4([4]byte{
			byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24),
		})
		rows = append(rows, pmRouteCandidate{addr: addr, metric: metric, iface: f[0]})
	}
	return pmSortRoutes(rows)
}

// pmProcRoute6 parses /proc/net/ipv6_route best-effort. Columns are
// dest/plen/src/srcplen/nexthop/metric/refcnt/use/flags/iface, all hex, with
// addresses written big-endian as 32 hex digits.
func pmProcRoute6(log *slog.Logger) []netip.Addr {
	data, err := os.ReadFile("/proc/net/ipv6_route")
	if err != nil {
		log.Debug("no /proc/net/ipv6_route", slog.String("error", err.Error()))
		return nil
	}
	var rows []pmRouteCandidate
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		plen, err := strconv.ParseUint(f[1], 16, 8)
		if err != nil || plen != 0 {
			continue
		}
		if strings.Trim(f[0], "0") != "" { // destination must be ::
			continue
		}
		flags, err := strconv.ParseUint(f[8], 16, 64)
		if err != nil || flags&pmRTFGateway == 0 {
			continue
		}
		nh, ok := pmHexAddr16(f[4])
		if !ok || !nh.IsValid() || nh.IsUnspecified() {
			continue
		}
		metric := 0
		if m, err := strconv.ParseUint(f[5], 16, 32); err == nil {
			metric = int(m)
		}
		rows = append(rows, pmRouteCandidate{addr: nh, metric: metric, iface: f[9]})
	}
	return pmSortRoutes(rows)
}

// pmHexAddr16 decodes 32 hex digits into an IPv6 address.
func pmHexAddr16(s string) (netip.Addr, bool) {
	if len(s) != 32 {
		return netip.Addr{}, false
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return netip.Addr{}, false
	}
	var b [16]byte
	copy(b[:], raw)
	return netip.AddrFrom16(b), true
}

// pmGuessedGateways offers x.x.x.1 and x.x.x.254 for every private IPv4 prefix
// on an up, non-loopback interface. Interfaces are visited in name order for
// determinism.
func pmGuessedGateways(ifaces []net.Interface) []netip.Addr {
	sorted := make([]net.Interface, len(ifaces))
	copy(sorted, ifaces)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var out []netip.Addr
	for _, ifc := range sorted {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			pfx, ok := pmPrefixOf(ipn)
			if !ok || !pfx.Addr().Is4() || !pfx.Addr().IsPrivate() {
				continue
			}
			if bits := pfx.Bits(); bits < 8 || bits > 30 {
				continue
			}
			base := pfx.Masked().Addr()
			out = append(out, base.Next()) // x.x.x.1
			if last, ok := pmLastHost(pfx); ok {
				out = append(out, last)
			}
		}
	}
	return out
}

// pmPrefixOf converts a net.IPNet to a netip.Prefix.
func pmPrefixOf(ipn *net.IPNet) (netip.Prefix, bool) {
	addr, ok := netip.AddrFromSlice(ipn.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	addr = addr.Unmap()
	ones, _ := ipn.Mask.Size()
	if ones == 0 && len(ipn.Mask) == 0 {
		return netip.Prefix{}, false
	}
	pfx, err := addr.Prefix(ones)
	if err != nil {
		return netip.Prefix{}, false
	}
	return pfx, true
}

// pmLastHost returns the conventional "high" gateway of an IPv4 prefix:
// x.x.x.254 for prefixes at least as large as a /24, otherwise the last usable
// address before the broadcast address.
func pmLastHost(pfx netip.Prefix) (netip.Addr, bool) {
	base := pfx.Masked().Addr()
	if !base.Is4() {
		return netip.Addr{}, false
	}
	b := base.As4()
	if pfx.Bits() <= 24 {
		b[3] = 254
		return netip.AddrFrom4(b), true
	}
	// Broadcast address of the prefix, minus one.
	host := uint32(1)<<(32-uint(pfx.Bits())) - 1
	v := binary.BigEndian.Uint32(b[:]) | host
	if v == 0 {
		return netip.Addr{}, false
	}
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v-1)
	return netip.AddrFrom4(out), true
}

// pmDedupAddrs removes duplicates and invalid entries, preserving order.
func pmDedupAddrs(in []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]bool, len(in))
	out := make([]netip.Addr, 0, len(in))
	for _, a := range in {
		a = a.Unmap()
		if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func pmJoinAddrs(in []netip.Addr) string {
	parts := make([]string, 0, len(in))
	for _, a := range in {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// the public probe
// ---------------------------------------------------------------------------

// ProbePortMapping probes UPnP IGD, NAT-PMP and PCP concurrently and reports
// what the router is willing to do for us.
//
// The three protocols are independent, so a hang in one cannot delay the
// others; the whole call is bounded both by ctx and by an internal six second
// budget. A router that answers nothing is [StatusWarn], not an error: it is a
// real (and common) finding for peer-to-peer traffic. [StatusFail] is reserved
// for the case where the local network stack could not even be enumerated.
func ProbePortMapping(ctx context.Context, logger *slog.Logger) PortMapReport {
	log := pmLog(logger)

	ctx, cancel := context.WithTimeout(ctx, pmBudget)
	defer cancel()

	var rep PortMapReport

	gws, err := pmGateways(ctx, log)
	if err != nil && len(gws) == 0 {
		rep.Status = StatusFail
		rep.Summary = "无法枚举本机网络接口，端口映射检测已跳过"
		rep.UPnP.Err = err.Error()
		rep.NATPMP.Err = err.Error()
		rep.PCP.Err = err.Error()
		log.Warn("port mapping probe aborted", slog.String("error", err.Error()))
		return rep
	}
	if len(gws) > 0 {
		rep.Gateway = gws[0]
	}

	var (
		mu       sync.Mutex
		answered netip.Addr
		wg       sync.WaitGroup
	)
	note := func(gw netip.Addr) {
		mu.Lock()
		if !answered.IsValid() && gw.IsValid() {
			answered = gw
		}
		mu.Unlock()
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		probe, gw := pmProbeNATPMP(ctx, gws, log)
		mu.Lock()
		rep.NATPMP = probe
		mu.Unlock()
		if probe.Available {
			note(gw)
		}
	}()
	go func() {
		defer wg.Done()
		probe, gw := pmProbePCP(ctx, gws, log)
		mu.Lock()
		rep.PCP = probe
		mu.Unlock()
		if probe.Available {
			note(gw)
		}
	}()
	go func() {
		defer wg.Done()
		probe := pmProbeUPnP(ctx, log)
		mu.Lock()
		rep.UPnP = probe
		mu.Unlock()
	}()
	wg.Wait()

	if answered.IsValid() {
		rep.Gateway = answered
	}
	pmSummarize(&rep)
	log.Info("port mapping probe done",
		slog.String("gateway", rep.Gateway.String()),
		slog.Bool("upnp", rep.UPnP.Available),
		slog.Bool("natpmp", rep.NATPMP.Available),
		slog.Bool("pcp", rep.PCP.Available),
		slog.String("status", rep.Status.String()))
	return rep
}

// pmSummarize fills Status and the one-line Chinese summary.
func pmSummarize(rep *PortMapReport) {
	var ok []string
	if rep.UPnP.Available {
		ok = append(ok, "UPnP")
	}
	if rep.NATPMP.Available {
		ok = append(ok, "NAT-PMP")
	}
	if rep.PCP.Available {
		ok = append(ok, "PCP")
	}

	gw := "未知网关"
	if rep.Gateway.IsValid() {
		gw = rep.Gateway.String()
	}
	ext := rep.UPnP.ExternalIP
	if !ext.IsValid() {
		ext = rep.NATPMP.ExternalIP
	}

	if len(ok) == 0 {
		rep.Status = StatusWarn
		rep.Summary = fmt.Sprintf("路由器 %s 未响应 UPnP / NAT-PMP / PCP，需要手动端口转发", gw)
		return
	}
	rep.Status = StatusOK
	if ext.IsValid() {
		rep.Summary = fmt.Sprintf("路由器 %s 支持 %s，可自动映射端口，外网地址 %s",
			gw, strings.Join(ok, " / "), ext)
		return
	}
	rep.Summary = fmt.Sprintf("路由器 %s 支持 %s，可自动映射端口", gw, strings.Join(ok, " / "))
}

// ---------------------------------------------------------------------------
// NAT-PMP (RFC 6886)
// ---------------------------------------------------------------------------

// pmProbeNATPMP asks each gateway candidate in turn for its external address
// and returns the first answer, along with the gateway that gave it.
func pmProbeNATPMP(ctx context.Context, gws []netip.Addr, log *slog.Logger) (ServiceProbe, netip.Addr) {
	var probe ServiceProbe
	if len(gws) == 0 {
		probe.Err = "no gateway candidate"
		return probe, netip.Addr{}
	}
	deadline := pmDeadline(ctx, pmProtoBudget)
	var lastErr error
	for _, gw := range gws {
		if !gw.Is4() { // NAT-PMP is IPv4-only
			continue
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		p, err := pmNATPMPOnce(ctx, gw, deadline, log)
		if err != nil {
			lastErr = err
			continue
		}
		return p, gw
	}
	if lastErr != nil {
		probe.Err = lastErr.Error()
	} else {
		probe.Err = "no IPv4 gateway candidate"
	}
	return probe, netip.Addr{}
}

// pmNATPMPOnce runs one external-address transaction against gw, retrying
// pmRetries times as RFC 6886 prescribes.
func pmNATPMPOnce(ctx context.Context, gw netip.Addr, deadline time.Time, log *slog.Logger) (ServiceProbe, error) {
	var probe ServiceProbe

	conn, err := pmDialUDP(ctx, gw)
	if err != nil {
		return probe, err
	}
	defer conn.Close()

	req := []byte{0x00, 0x00} // version 0, opcode 0 = public address request
	buf := make([]byte, 64)

	// start is per attempt, not per probe: timing from before the retry loop
	// would add every prior attempt's full timeout to the reported round trip,
	// so a router that answers on the third try would look ~1.5s away.
	var start time.Time

	for attempt := 0; attempt <= pmRetries; attempt++ {
		if ctx.Err() != nil {
			return probe, ctx.Err()
		}
		until := time.Now().Add(pmUnicastTimeout)
		if until.After(deadline) {
			until = deadline
		}
		if !until.After(time.Now()) {
			break
		}
		start = time.Now()
		if _, err = conn.Write(req); err != nil {
			return probe, err
		}
		_ = conn.SetReadDeadline(until)
		n, rerr := conn.Read(buf)
		if rerr != nil {
			err = rerr
			log.Debug("nat-pmp attempt timed out",
				slog.String("gateway", gw.String()), slog.Int("attempt", attempt+1))
			continue
		}
		rtt := time.Since(start)
		if n < 12 {
			err = fmt.Errorf("short nat-pmp response: %d bytes", n)
			continue
		}
		if buf[0] != 0 || buf[1] != 0x80 {
			err = fmt.Errorf("unexpected nat-pmp header %#x/%#x", buf[0], buf[1])
			continue
		}
		code := binary.BigEndian.Uint16(buf[2:4])
		epoch := binary.BigEndian.Uint32(buf[4:8])
		if code != 0 {
			probe.RTT = rtt
			probe.Err = fmt.Sprintf("NAT-PMP v0 result code %d (%s)", code, pmPMPResultName(code))
			probe.Detail = fmt.Sprintf("NAT-PMP v0 @ %s, epoch %ds", gw, epoch)
			return probe, nil
		}
		var ip4 [4]byte
		copy(ip4[:], buf[8:12])
		probe.Available = true
		probe.RTT = rtt
		probe.ExternalIP = netip.AddrFrom4(ip4)
		probe.Detail = fmt.Sprintf("NAT-PMP v0 @ %s, epoch %ds (路由器已运行 %s)",
			gw, epoch, pmHuman(time.Duration(epoch)*time.Second))
		return probe, nil
	}
	if err == nil {
		err = errors.New("no nat-pmp response")
	}
	return probe, err
}

// pmPMPResultName names the RFC 6886 result codes.
func pmPMPResultName(code uint16) string {
	switch code {
	case 1:
		return "unsupported version"
	case 2:
		return "not authorized / refused"
	case 3:
		return "network failure"
	case 4:
		return "out of resources"
	case 5:
		return "unsupported opcode"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// PCP (RFC 6887)
// ---------------------------------------------------------------------------

// pmProbePCP sends a PCP ANNOUNCE to each gateway candidate and returns the
// first conclusive answer.
//
// PCP shares UDP/5351 with NAT-PMP, so a version-0 reply (or a version-2
// UNSUPP_VERSION result) means "this router speaks NAT-PMP but not PCP" rather
// than "nothing is there"; that distinction is recorded in Detail.
func pmProbePCP(ctx context.Context, gws []netip.Addr, log *slog.Logger) (ServiceProbe, netip.Addr) {
	var probe ServiceProbe
	if len(gws) == 0 {
		probe.Err = "no gateway candidate"
		return probe, netip.Addr{}
	}
	deadline := pmDeadline(ctx, pmProtoBudget)
	var (
		lastErr  error
		fallback *ServiceProbe
	)
	for _, gw := range gws {
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		p, err := pmPCPOnce(ctx, gw, deadline, log)
		if err != nil {
			lastErr = err
			continue
		}
		if p.Available {
			return p, gw
		}
		if fallback == nil {
			cp := p
			fallback = &cp
		}
	}
	if fallback != nil {
		return *fallback, netip.Addr{}
	}
	if lastErr != nil {
		probe.Err = lastErr.Error()
	} else {
		probe.Err = "no pcp response"
	}
	return probe, netip.Addr{}
}

// pmPCPOnce runs one ANNOUNCE transaction against gw.
func pmPCPOnce(ctx context.Context, gw netip.Addr, deadline time.Time, log *slog.Logger) (ServiceProbe, error) {
	var probe ServiceProbe

	conn, err := pmDialUDP(ctx, gw)
	if err != nil {
		return probe, err
	}
	defer conn.Close()

	local, ok := netip.AddrFromSlice(conn.LocalAddr().(*net.UDPAddr).IP)
	if !ok {
		return probe, errors.New("cannot determine local address for pcp")
	}
	client := local.Unmap().As16() // IPv4-mapped IPv6 for v4 sources

	req := make([]byte, 24)
	req[0] = 2    // version
	req[1] = 0x00 // R=0 (request), opcode 0 = ANNOUNCE
	// req[2:4] reserved, req[4:8] requested lifetime = 0
	copy(req[8:24], client[:])

	buf := make([]byte, 1100)

	// Per attempt; see the note in pmNATPMPOnce.
	var start time.Time

	for attempt := 0; attempt <= pmRetries; attempt++ {
		if ctx.Err() != nil {
			return probe, ctx.Err()
		}
		until := time.Now().Add(pmUnicastTimeout)
		if until.After(deadline) {
			until = deadline
		}
		if !until.After(time.Now()) {
			break
		}
		start = time.Now()
		if _, err = conn.Write(req); err != nil {
			return probe, err
		}
		_ = conn.SetReadDeadline(until)
		n, rerr := conn.Read(buf)
		if rerr != nil {
			err = rerr
			log.Debug("pcp attempt timed out",
				slog.String("gateway", gw.String()), slog.Int("attempt", attempt+1))
			continue
		}
		rtt := time.Since(start)

		// A NAT-PMP-speaking router answers a PCP request with version 0.
		if n >= 4 && buf[0] == 0 {
			probe.RTT = rtt
			probe.Detail = fmt.Sprintf("%s 回应 NAT-PMP v0，不支持 PCP", gw)
			probe.Err = "gateway answered NAT-PMP v0, PCP unsupported"
			return probe, nil
		}
		if n < 24 {
			err = fmt.Errorf("short pcp response: %d bytes", n)
			continue
		}
		if buf[0] != 2 || buf[1] != 0x80 {
			err = fmt.Errorf("unexpected pcp header %#x/%#x", buf[0], buf[1])
			continue
		}
		code := buf[3]
		epoch := binary.BigEndian.Uint32(buf[8:12])
		if code == 1 { // UNSUPP_VERSION
			probe.RTT = rtt
			probe.Detail = fmt.Sprintf("%s 返回 UNSUPP_VERSION，仅支持 NAT-PMP，不支持 PCP v2", gw)
			probe.Err = "pcp result code 1 (unsupported version); NAT-PMP present"
			return probe, nil
		}
		if code != 0 {
			probe.RTT = rtt
			probe.Detail = fmt.Sprintf("PCP v2 @ %s, epoch %ds", gw, epoch)
			probe.Err = fmt.Sprintf("PCP v2 result code %d (%s)", code, pmPCPResultName(code))
			return probe, nil
		}
		probe.Available = true
		probe.RTT = rtt
		probe.Detail = fmt.Sprintf("PCP v2 @ %s, epoch %ds (路由器已运行 %s)",
			gw, epoch, pmHuman(time.Duration(epoch)*time.Second))
		return probe, nil
	}
	if err == nil {
		err = errors.New("no pcp response")
	}
	return probe, err
}

// pmPCPResultName names the RFC 6887 result codes we are likely to see.
func pmPCPResultName(code byte) string {
	switch code {
	case 1:
		return "unsupported version"
	case 2:
		return "not authorized"
	case 3:
		return "malformed request"
	case 4:
		return "unsupported opcode"
	case 5:
		return "unsupported option"
	case 6:
		return "malformed option"
	case 7:
		return "network failure"
	case 8:
		return "no resources"
	case 9:
		return "unsupported protocol"
	case 10:
		return "user exceeded quota"
	case 11:
		return "cannot provide external"
	case 12:
		return "address mismatch"
	case 13:
		return "excessive remote peers"
	default:
		return "unknown"
	}
}

// pmDialUDP opens a connected UDP socket towards gw:5351, honouring ctx.
func pmDialUDP(ctx context.Context, gw netip.Addr) (net.Conn, error) {
	d := net.Dialer{Timeout: pmUnicastTimeout}
	network := "udp4"
	if gw.Is6() {
		network = "udp6"
	}
	return d.DialContext(ctx, network, netip.AddrPortFrom(gw, pmPort).String())
}

// pmHuman renders a duration the way a router uptime reads.
func pmHuman(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, h)
	}
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// ---------------------------------------------------------------------------
// UPnP IGD
// ---------------------------------------------------------------------------

// pmProbeUPnP runs SSDP discovery, fetches the device description and asks the
// WAN connection service for the external address.
//
// Available flips to true as soon as an IGD answered SSDP and its description
// parsed; a failing SOAP call only fills Err, because a discoverable IGD is
// still a useful finding.
func pmProbeUPnP(ctx context.Context, log *slog.Logger) ServiceProbe {
	var probe ServiceProbe
	start := time.Now()

	locations := pmSSDPDiscover(ctx, "urn:schemas-upnp-org:device:InternetGatewayDevice:1", log)
	if len(locations) == 0 && ctx.Err() == nil {
		log.Debug("ssdp igd search empty, retrying with ssdp:all")
		locations = pmSSDPDiscover(ctx, "ssdp:all", log)
	}
	if len(locations) == 0 {
		probe.Err = "no ssdp response"
		return probe
	}
	if len(locations) > pmMaxLocations {
		locations = locations[:pmMaxLocations]
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil, // the router is local; never go through a proxy
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: pmHTTPTimeout,
		},
	}

	var lastErr error
	for _, loc := range locations {
		if ctx.Err() != nil {
			break
		}
		dev, base, err := pmFetchDescription(ctx, client, loc, log)
		if err != nil {
			lastErr = err
			continue
		}
		probe.Available = true
		probe.RTT = time.Since(start)
		probe.Detail = pmDeviceLabel(dev, loc)

		svcType, ctrlURL, ok := pmFindWANService(dev, base)
		if !ok {
			probe.Err = "no WANIPConnection/WANPPPConnection service in device description"
			return probe
		}
		probe.Detail = pmDeviceLabel(dev, ctrlURL)

		ip, err := pmSOAPExternalIP(ctx, client, ctrlURL, svcType)
		if err != nil {
			probe.Err = "GetExternalIPAddress: " + err.Error()
			return probe
		}
		probe.ExternalIP = ip
		probe.RTT = time.Since(start)
		return probe
	}
	if lastErr != nil {
		probe.Err = lastErr.Error()
	} else {
		probe.Err = "no usable igd description"
	}
	return probe
}

// pmDeviceLabel renders "<friendlyName> (<modelName>)" when known, falling
// back to the manufacturer or the URL.
func pmDeviceLabel(dev *pmUPnPDevice, fallback string) string {
	name := strings.TrimSpace(dev.FriendlyName)
	model := strings.TrimSpace(dev.ModelName)
	switch {
	case name != "" && model != "":
		return fmt.Sprintf("%s (%s)", name, model)
	case name != "":
		return name
	case model != "":
		return model
	case strings.TrimSpace(dev.Manufacturer) != "":
		return strings.TrimSpace(dev.Manufacturer)
	default:
		return fallback
	}
}

// pmSSDPDiscover sends an M-SEARCH from every usable IPv4 interface address and
// collects the LOCATION headers of the replies for roughly two seconds. The
// returned list is deduplicated and sorted so the UI does not jitter.
func pmSSDPDiscover(ctx context.Context, st string, log *slog.Logger) []string {
	srcs := pmSSDPSources(log)
	if len(srcs) == 0 {
		return nil
	}
	body := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: " + strconv.Itoa(pmSSDPMX) + "\r\n" +
		"ST: " + st + "\r\n" +
		"\r\n"

	target, err := net.ResolveUDPAddr("udp4", pmSSDPTarget)
	if err != nil {
		log.Debug("cannot resolve ssdp target", slog.String("error", err.Error()))
		return nil
	}
	deadline := pmDeadline(ctx, pmSSDPCollect)

	var (
		mu   sync.Mutex
		locs = map[string]bool{}
		wg   sync.WaitGroup
		sem  = make(chan struct{}, pmMaxSSDPSockets)
	)
	for _, src := range srcs {
		wg.Add(1)
		go func(src netip.Addr) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for _, l := range pmSSDPOne(src, target, body, deadline, log) {
				mu.Lock()
				locs[l] = true
				mu.Unlock()
			}
		}(src)
	}
	wg.Wait()

	out := make([]string, 0, len(locs))
	for l := range locs {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// pmSSDPOne performs the M-SEARCH from a single source address. Interfaces
// without multicast support fail here routinely; that is a debug-level event,
// not an error worth surfacing.
func pmSSDPOne(src netip.Addr, target *net.UDPAddr, body string, hardDeadline time.Time, log *slog.Logger) []string {
	if !hardDeadline.After(time.Now()) {
		return nil
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IP(src.AsSlice())})
	if err != nil {
		log.Debug("ssdp socket unavailable",
			slog.String("src", src.String()), slog.String("error", err.Error()))
		return nil
	}
	defer conn.Close()

	if _, err := conn.WriteToUDP([]byte(body), target); err != nil {
		log.Debug("ssdp multicast send failed",
			slog.String("src", src.String()), slog.String("error", err.Error()))
		return nil
	}

	// Give devices the full MX window measured from the send, not from when
	// the caller computed a shared deadline: socket setup and goroutine
	// scheduling happen in between, and every millisecond of that came out of
	// the reply window.
	deadline := time.Now().Add(pmSSDPCollect)
	if deadline.After(hardDeadline) {
		deadline = hardDeadline
	}

	var out []string
	buf := make([]byte, 4096)
	for {
		if !deadline.After(time.Now()) {
			break
		}
		_ = conn.SetReadDeadline(deadline)
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if loc := pmSSDPLocation(buf[:n]); loc != "" {
			log.Debug("ssdp reply",
				slog.String("from", from.String()), slog.String("location", loc))
			out = append(out, loc)
		}
	}
	return out
}

// pmSSDPLocation extracts the LOCATION header from an SSDP reply,
// case-insensitively.
func pmSSDPLocation(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "location") {
			continue
		}
		v = strings.TrimSpace(v)
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return ""
		}
		return v
	}
	return ""
}

// pmSSDPSources lists the IPv4 addresses worth sending an M-SEARCH from.
func pmSSDPSources(log *slog.Logger) []netip.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Debug("cannot enumerate interfaces for ssdp", slog.String("error", err.Error()))
		return nil
	}
	sort.Slice(ifaces, func(i, j int) bool { return ifaces[i].Name < ifaces[j].Name })

	var out []netip.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if !addr.Is4() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, addr)
		}
	}
	return pmDedupAddrs(out)
}

// ---------------------------------------------------------------------------
// device description
// ---------------------------------------------------------------------------

// pmUPnPService is one service entry of a UPnP device description.
type pmUPnPService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

// pmUPnPDevice is one (possibly nested) device entry of a UPnP description.
type pmUPnPDevice struct {
	DeviceType   string          `xml:"deviceType"`
	FriendlyName string          `xml:"friendlyName"`
	Manufacturer string          `xml:"manufacturer"`
	ModelName    string          `xml:"modelName"`
	Services     []pmUPnPService `xml:"serviceList>service"`
	Devices      []pmUPnPDevice  `xml:"deviceList>device"`
}

// pmUPnPRoot is the root element of a UPnP device description.
type pmUPnPRoot struct {
	URLBase string       `xml:"URLBase"`
	Device  pmUPnPDevice `xml:"device"`
}

// pmFetchDescription GETs a LOCATION and parses the device description. It
// returns the root device and the base URL that relative control URLs resolve
// against.
func pmFetchDescription(ctx context.Context, client *http.Client, loc string, log *slog.Logger) (*pmUPnPDevice, *url.URL, error) {
	rctx, cancel := context.WithTimeout(ctx, pmHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, loc, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("description %s: http %d", loc, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, pmMaxDescBody))
	if err != nil {
		return nil, nil, err
	}

	var root pmUPnPRoot
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = pmPassthroughCharset
	dec.Strict = false
	if err := dec.Decode(&root); err != nil {
		return nil, nil, fmt.Errorf("parse description %s: %w", loc, err)
	}

	base, err := url.Parse(loc)
	if err != nil {
		return nil, nil, err
	}
	// URLBase comes from a device description fetched after an unauthenticated
	// multicast handshake, i.e. it is attacker-controlled on a hostile LAN.
	// Accepting it unchecked would let any device on the network redirect the
	// SOAP POST below to a host of its choosing — including internal services
	// the router itself cannot reach. Only honour a rebase that stays on the
	// same origin we already decided to talk to.
	if b := strings.TrimSpace(root.URLBase); b != "" {
		if u, err := url.Parse(b); err == nil && pmSameOrigin(base, u) {
			base = u
		} else {
			log.Debug("ignoring off-origin URLBase",
				slog.String("location", loc), slog.String("urlbase", b))
		}
	}
	log.Debug("igd description parsed",
		slog.String("location", loc), slog.String("device", root.Device.FriendlyName))
	return &root.Device, base, nil
}

// pmSameOrigin reports whether u is an http(s) URL on the same host as base.
// The port may differ — IGDs routinely serve the description and the control
// endpoint on different ports — but the host may not.
func pmSameOrigin(base, u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Hostname(), base.Hostname())
}

// pmPassthroughCharset accepts non-UTF-8 declarations rather than failing the
// parse; router descriptions are ASCII in practice.
func pmPassthroughCharset(_ string, input io.Reader) (io.Reader, error) {
	return input, nil
}

// pmFindWANService walks the nested device tree for a WAN connection service
// and resolves its control URL against base.
func pmFindWANService(dev *pmUPnPDevice, base *url.URL) (svcType, ctrlURL string, ok bool) {
	for _, want := range pmWANServices {
		if st, cu, found := pmFindService(dev, want); found {
			ref, err := url.Parse(strings.TrimSpace(cu))
			if err != nil {
				continue
			}
			return st, base.ResolveReference(ref).String(), true
		}
	}
	return "", "", false
}

// pmFindService searches dev and its children for an exact serviceType match.
func pmFindService(dev *pmUPnPDevice, want string) (svcType, ctrlURL string, ok bool) {
	for _, s := range dev.Services {
		if strings.EqualFold(strings.TrimSpace(s.ServiceType), want) && strings.TrimSpace(s.ControlURL) != "" {
			return strings.TrimSpace(s.ServiceType), s.ControlURL, true
		}
	}
	for i := range dev.Devices {
		if st, cu, found := pmFindService(&dev.Devices[i], want); found {
			return st, cu, true
		}
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// SOAP
// ---------------------------------------------------------------------------

// pmSOAPExternalIP invokes GetExternalIPAddress on an IGD control URL.
func pmSOAPExternalIP(ctx context.Context, client *http.Client, ctrlURL, svcType string) (netip.Addr, error) {
	envelope := `<?xml version="1.0"?>` + "\r\n" +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:GetExternalIPAddress xmlns:u="` + svcType + `"></u:GetExternalIPAddress></s:Body>` +
		`</s:Envelope>` + "\r\n"

	rctx, cancel := context.WithTimeout(ctx, pmHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, ctrlURL, strings.NewReader(envelope))
	if err != nil {
		return netip.Addr{}, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+svcType+`#GetExternalIPAddress"`)

	resp, err := client.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, pmMaxDescBody))
	if err != nil {
		return netip.Addr{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("http %d", resp.StatusCode)
	}
	raw := pmXMLValue(body, "NewExternalIPAddress")
	if raw == "" {
		return netip.Addr{}, errors.New("no NewExternalIPAddress in soap response")
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("bad external address %q", raw)
	}
	return addr.Unmap(), nil
}

// pmXMLValue returns the character data of the first element with the given
// local name, ignoring namespaces.
func pmXMLValue(body []byte, local string) string {
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = pmPassthroughCharset
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != local {
			continue
		}
		var v string
		if err := dec.DecodeElement(&v, &se); err != nil {
			return ""
		}
		return v
	}
}
