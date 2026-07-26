package netdiag

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// ifDialTimeout bounds each source-address discovery dial. The dial is to a
// UDP address, so no packet leaves the machine and the kernel answers from its
// routing table immediately; the timeout only guards against a pathological
// resolver or a wedged network stack.
const ifDialTimeout = 2 * time.Second

// ifMaxInflight bounds how many interfaces are inspected concurrently.
const ifMaxInflight = 8

// ifDefaultV4Target and ifDefaultV6Target are well-known anycast resolvers used
// purely as "somewhere on the default route" destinations.
const (
	ifDefaultV4Target = "8.8.8.8:80"
	ifDefaultV6Target = "[2001:4860:4860::8888]:80"
)

// tailscaleV6Prefix is the ULA range Tailscale assigns to every node.
var tailscaleV6Prefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// cgnatPrefix is RFC 6598 shared address space. Tailscale allocates its IPv4
// node addresses out of 100.64.0.0/10 as well, which is why an address here
// needs the interface name to be classified precisely; see [classifyOnIface].
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

var (
	ulaPrefix       = netip.MustParsePrefix("fc00::/7")
	linkLocalV4Pfx  = netip.MustParsePrefix("169.254.0.0/16")
	rfc1918Prefixes = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
)

// ifTailscaleIfaceNames are the interface-name prefixes Tailscale (and the
// wireguard/utun devices it rides on) uses across platforms.
var ifTailscaleIfaceNames = []string{"tailscale", "ts", "utun", "wg"}

func ifLog(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(slog.String("from", "netdiag/iface"))
}

// ClassifyAddr buckets an address by reachable scope.
//
// Addresses in 100.64.0.0/10 are reported as [AddrCGNAT] because the range
// alone cannot distinguish a carrier-grade NAT lease from a Tailscale node
// address. [EnumerateInterfaces] refines that verdict using the interface name.
func ClassifyAddr(a netip.Addr) AddrKind {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return AddrLinkLocal // degenerate input; never reachable
	case a.IsLoopback():
		return AddrLoopback
	case a.Is4() && cgnatPrefix.Contains(a):
		return AddrCGNAT
	case a.Is6() && tailscaleV6Prefix.Contains(a):
		return AddrTailscale
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || (a.Is4() && linkLocalV4Pfx.Contains(a)):
		return AddrLinkLocal
	case a.Is4() && isRFC1918(a):
		return AddrPrivateV4
	case a.Is6() && ulaPrefix.Contains(a):
		return AddrULA
	case a.Is4():
		return AddrGlobalV4
	default:
		return AddrGlobalV6
	}
}

func isRFC1918(a netip.Addr) bool {
	for _, p := range rfc1918Prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// classifyOnIface applies [ClassifyAddr] and then corrects the one case the
// address alone cannot decide: Tailscale hands out IPv4 addresses from the
// CGNAT range 100.64.0.0/10, so a 100.x address sitting on an interface named
// tailscale*/ts*/utun*/wg* is a tailnet address rather than a carrier NAT
// lease. The heuristic is name-based because the alternative (asking tailscaled)
// would make this package depend on tailscale.com.
func classifyOnIface(a netip.Addr, iface string) AddrKind {
	kind := ClassifyAddr(a)
	if kind == AddrCGNAT && isTailscaleIfaceName(iface) {
		return AddrTailscale
	}
	return kind
}

func isTailscaleIfaceName(name string) bool {
	n := strings.ToLower(name)
	for _, p := range ifTailscaleIfaceNames {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// EnumerateInterfaces lists every address bound to every local interface and
// determines which source addresses the kernel would use for default routes.
func EnumerateInterfaces(ctx context.Context, logger *slog.Logger) InterfaceReport {
	log := ifLog(logger)
	var rep InterfaceReport

	ifaces, err := net.Interfaces()
	if err != nil {
		log.With(slog.String("error", err.Error())).Error("failed to enumerate interfaces")
		rep.Err = err.Error()
		rep.Status = StatusFail
		rep.Summary = "无法枚举本机网络接口"
		return rep
	}

	var (
		mu     sync.Mutex
		addrs  []LocalAddr
		nIface int
	)

	sem := make(chan struct{}, ifMaxInflight)
	var wg sync.WaitGroup

	// Source discovery is independent of enumeration, so run both in parallel.
	var v4Src, v6Src netip.Addr
	wg.Add(2)
	go func() {
		defer wg.Done()
		v4Src = defaultSource(ctx, "udp4", ifDefaultV4Target, log)
	}()
	go func() {
		defer wg.Done()
		v6Src = defaultSource(ctx, "udp6", ifDefaultV6Target, log)
	}()

	for _, iface := range ifaces {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(iface net.Interface) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			got := ifaceAddrs(iface, log)
			mu.Lock()
			if len(got) > 0 {
				nIface++
			}
			addrs = append(addrs, got...)
			mu.Unlock()
		}(iface)
	}
	wg.Wait()

	rep.Addrs = addrs
	rep.DefaultV4Src = v4Src
	rep.DefaultV6Src = v6Src

	sortLocalAddrs(rep.Addrs)

	hasGlobalV6Addr := false
	for i := range rep.Addrs {
		a := &rep.Addrs[i]
		if a.Kind == AddrGlobalV6 {
			hasGlobalV6Addr = true
		}
		if (v4Src.IsValid() && a.Addr == v4Src) || (v6Src.IsValid() && a.Addr == v6Src) {
			a.IsDefaultSrc = true
		}
	}
	rep.HasGlobalV6 = hasGlobalV6Addr && v6Src.IsValid() && ClassifyAddr(v6Src) == AddrGlobalV6

	finishInterfaceReport(&rep, nIface)

	log.With(
		slog.Int("interfaces", nIface),
		slog.Int("addrs", len(rep.Addrs)),
		slog.String("v4_src", addrText(rep.DefaultV4Src)),
		slog.String("v6_src", addrText(rep.DefaultV6Src)),
		slog.String("status", rep.Status.String()),
	).Debug("enumerated local interfaces")

	return rep
}

// ifaceAddrs converts one interface's bound addresses into [LocalAddr] entries.
// Errors are logged and swallowed: one unreadable interface must not blank the
// whole panel.
func ifaceAddrs(iface net.Interface, log *slog.Logger) []LocalAddr {
	raw, err := iface.Addrs()
	if err != nil {
		log.With(
			slog.String("iface", iface.Name),
			slog.String("error", err.Error()),
		).Debug("failed to read interface addresses")
		return nil
	}

	hw := ""
	if len(iface.HardwareAddr) > 0 {
		hw = strings.ToLower(iface.HardwareAddr.String())
	}
	up := iface.Flags&net.FlagUp != 0

	out := make([]LocalAddr, 0, len(raw))
	for _, a := range raw {
		pfx, ok := toPrefix(a)
		if !ok {
			continue
		}
		addr := pfx.Addr().Unmap()
		// Keep the zone off the reported address so equality against the
		// default-source lookup and the sort order stay stable.
		addr = addr.WithZone("")
		out = append(out, LocalAddr{
			Iface:    iface.Name,
			Addr:     addr,
			Prefix:   netip.PrefixFrom(addr, pfx.Bits()),
			Kind:     classifyOnIface(addr, iface.Name),
			Up:       up,
			MTU:      iface.MTU,
			Hardware: hw,
		})
	}
	return out
}

// toPrefix normalises the net.Addr values iface.Addrs returns (*net.IPNet on
// every supported platform, *net.IPAddr on a few).
func toPrefix(a net.Addr) (netip.Prefix, bool) {
	switch v := a.(type) {
	case *net.IPNet:
		addr, ok := netip.AddrFromSlice(v.IP)
		if !ok {
			return netip.Prefix{}, false
		}
		addr = addr.Unmap()
		ones, _ := v.Mask.Size()
		if ones <= 0 || ones > addr.BitLen() {
			ones = addr.BitLen()
		}
		return netip.PrefixFrom(addr, ones), true
	case *net.IPAddr:
		addr, ok := netip.AddrFromSlice(v.IP)
		if !ok {
			return netip.Prefix{}, false
		}
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), true
	default:
		addr, err := netip.ParsePrefix(a.String())
		if err != nil {
			return netip.Prefix{}, false
		}
		return addr, true
	}
}

// defaultSource asks the kernel which local address it would use to reach a
// destination on the default route. Dialling a UDP address only installs a
// route lookup on the socket; nothing is transmitted. Failure is expected and
// normal (notably for udp6 on IPv4-only hosts) and never populates Err.
func defaultSource(ctx context.Context, network, target string, log *slog.Logger) netip.Addr {
	dctx, cancel := context.WithTimeout(ctx, ifDialTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dctx, network, target)
	if err != nil {
		log.With(
			slog.String("network", network),
			slog.String("error", err.Error()),
		).Debug("no default source address")
		return netip.Addr{}
	}
	defer conn.Close()

	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}
	}
	addr, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.Addr{}
	}
	return addr.Unmap().WithZone("")
}

// sortLocalAddrs orders entries by interface name, then IPv4 before IPv6, then
// by address, so repeated refreshes render identically.
func sortLocalAddrs(as []LocalAddr) {
	sort.Slice(as, func(i, j int) bool {
		x, y := as[i], as[j]
		if x.Iface != y.Iface {
			return x.Iface < y.Iface
		}
		if x.Addr.Is4() != y.Addr.Is4() {
			return x.Addr.Is4()
		}
		return x.Addr.Compare(y.Addr) < 0
	})
}

// finishInterfaceReport derives Status and Summary from the collected data.
func finishInterfaceReport(rep *InterfaceReport, nIface int) {
	if len(rep.Addrs) == 0 {
		rep.Status = StatusFail
		if rep.Err == "" {
			rep.Err = "no local addresses found"
		}
		rep.Summary = "未发现任何本机地址"
		return
	}

	// A private v4 address still routes out through NAT, but only if the kernel
	// actually picked a default source for it.
	var globalCapable bool
	for _, a := range rep.Addrs {
		switch a.Kind {
		case AddrGlobalV4, AddrGlobalV6, AddrCGNAT, AddrTailscale:
			globalCapable = true
		case AddrPrivateV4:
			globalCapable = globalCapable || rep.DefaultV4Src.IsValid()
		}
	}
	if globalCapable {
		rep.Status = StatusOK
	} else {
		rep.Status = StatusWarn
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d 个接口 / %d 个地址", nIface, len(rep.Addrs))
	if rep.DefaultV4Src.IsValid() {
		fmt.Fprintf(&b, "，IPv4 出口 %s", rep.DefaultV4Src)
	} else {
		b.WriteString("，无 IPv4 出口")
	}
	if rep.DefaultV6Src.IsValid() {
		fmt.Fprintf(&b, "，IPv6 出口 %s", rep.DefaultV6Src)
	} else {
		b.WriteString("，无 IPv6 出口")
	}
	rep.Summary = b.String()
}

// addrText renders an address for logging, using "-" for the invalid zero
// value so log lines stay readable.
func addrText(a netip.Addr) string {
	if !a.IsValid() {
		return "-"
	}
	return a.String()
}
