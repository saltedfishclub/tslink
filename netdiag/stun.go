package netdiag

// A minimal STUN implementation (RFC 5389) plus the behaviour-discovery bits of
// RFC 5780. tailscale.com/net/stun is deliberately not used: it parses binding
// responses only and has no CHANGE-REQUEST or OTHER-ADDRESS support, which are
// exactly what NAT filtering classification needs.
//
// Everything here parses hostile input from the open internet, so the parser
// skips attributes it does not know and returns errors — never panics — on
// truncated ones.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// wire format
// ---------------------------------------------------------------------------

const (
	stunHeaderSize = 20
	stunMaxMessage = 1500
	// stunMagicCookie is the fixed cookie every RFC 5389 message carries; it is
	// also the XOR key for XOR-MAPPED-ADDRESS.
	stunMagicCookie uint32 = 0x2112A442
)

// message types
const (
	stunBindingRequest uint16 = 0x0001
	stunBindingSuccess uint16 = 0x0101
	stunBindingError   uint16 = 0x0111
)

// attribute types
const (
	stunAttrMappedAddress    uint16 = 0x0001
	stunAttrChangeRequest    uint16 = 0x0003
	stunAttrSourceAddress    uint16 = 0x0004
	stunAttrChangedAddress   uint16 = 0x0005
	stunAttrErrorCode        uint16 = 0x0009
	stunAttrXORMappedAddress uint16 = 0x0020
	stunAttrXORMappedAddrAlt uint16 = 0x8020 // legacy/vendor duplicate
	stunAttrSoftware         uint16 = 0x8022
	stunAttrFingerprint      uint16 = 0x8028
	stunAttrResponseOrigin   uint16 = 0x802b
	stunAttrOtherAddress     uint16 = 0x802c
)

// CHANGE-REQUEST flags.
const (
	stunChangeIP   byte = 0x04
	stunChangePort byte = 0x02
)

// address families used by address attributes
const (
	stunFamilyV4 byte = 0x01
	stunFamilyV6 byte = 0x02
)

// timing and concurrency limits. Nothing here may block a GUI panel, so every
// wait is bounded twice: by these constants and by the caller's context.
const (
	stunAttempts       = 3
	stunInterval       = 700 * time.Millisecond
	stunQuickAttempts  = 2
	stunQuickInterval  = 600 * time.Millisecond
	stunProbeTimeout   = 3 * time.Second
	stunResolveTimeout = 3 * time.Second
	stunHairpinTimeout = 1500 * time.Millisecond
	stunMaxInFlight    = 8
	natTotalBudget     = 20 * time.Second
)

var (
	errSTUNTruncated = errors.New("stun: truncated message")
	errSTUNBadCookie = errors.New("stun: bad magic cookie")
	errSTUNTimeout   = errors.New("stun: no response")
	errSTUNFamily    = errors.New("stun: unsupported address family")
	errSTUNNoAddr    = errors.New("stun: host resolved to no addresses")
)

// stunAttr is one type-length-value attribute. Value excludes the padding that
// aligns the attribute to a 4-byte boundary on the wire.
type stunAttr struct {
	Type  uint16
	Value []byte
}

// stunMessage is a decoded STUN message.
type stunMessage struct {
	Type  uint16
	TxID  [12]byte
	Attrs []stunAttr
}

// encode serialises the message, padding every attribute to a 4-byte boundary
// with zeroes while keeping the declared length free of that padding.
func (m *stunMessage) encode() []byte {
	body := make([]byte, 0, 64)
	var hdr [4]byte
	for _, a := range m.Attrs {
		binary.BigEndian.PutUint16(hdr[0:2], a.Type)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(a.Value)))
		body = append(body, hdr[:]...)
		body = append(body, a.Value...)
		if pad := (4 - len(a.Value)%4) % 4; pad > 0 {
			body = append(body, make([]byte, pad)...)
		}
	}
	out := make([]byte, stunHeaderSize, stunHeaderSize+len(body))
	binary.BigEndian.PutUint16(out[0:2], m.Type)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(body)))
	binary.BigEndian.PutUint32(out[4:8], stunMagicCookie)
	copy(out[8:20], m.TxID[:])
	return append(out, body...)
}

// parseSTUNMessage decodes raw. Unknown attributes are kept but never
// interpreted; a truncated header or attribute is an error rather than a panic,
// because this runs on unauthenticated packets from the internet.
func parseSTUNMessage(raw []byte) (*stunMessage, error) {
	if len(raw) < stunHeaderSize {
		return nil, errSTUNTruncated
	}
	msgLen := int(binary.BigEndian.Uint16(raw[2:4]))
	if len(raw)-stunHeaderSize < msgLen {
		return nil, errSTUNTruncated
	}
	// RFC 5389 §6 requires the magic cookie. Without this check any 20-byte
	// datagram whose bytes 8..20 happen to match our transaction ID would be
	// accepted, and its XOR-MAPPED-ADDRESS would be de-XORed with a key the
	// sender never used — producing a silently wrong reflexive address rather
	// than an error.
	if binary.BigEndian.Uint32(raw[4:8]) != stunMagicCookie {
		return nil, errSTUNBadCookie
	}
	m := &stunMessage{Type: binary.BigEndian.Uint16(raw[0:2])}
	copy(m.TxID[:], raw[8:20])

	body := raw[stunHeaderSize : stunHeaderSize+msgLen]
	for off := 0; off < len(body); {
		if len(body)-off < 4 {
			return nil, errSTUNTruncated
		}
		typ := binary.BigEndian.Uint16(body[off : off+2])
		vlen := int(binary.BigEndian.Uint16(body[off+2 : off+4]))
		off += 4
		if len(body)-off < vlen {
			return nil, errSTUNTruncated
		}
		val := make([]byte, vlen)
		copy(val, body[off:off+vlen])
		m.Attrs = append(m.Attrs, stunAttr{Type: typ, Value: val})
		off += vlen
		// Missing trailing padding on the last attribute is tolerated: some
		// servers omit it and the message is still perfectly usable.
		if pad := (4 - vlen%4) % 4; pad > 0 {
			if len(body)-off < pad {
				break
			}
			off += pad
		}
	}
	return m, nil
}

// attr returns the value of the first attribute of type t.
func (m *stunMessage) attr(t uint16) ([]byte, bool) {
	for _, a := range m.Attrs {
		if a.Type == t {
			return a.Value, true
		}
	}
	return nil, false
}

// mappedAddr returns the server-reflexive address, preferring the XOR forms
// (which survive NATs that rewrite payloads) over the plain one.
func (m *stunMessage) mappedAddr() (netip.AddrPort, bool) {
	for _, t := range []uint16{stunAttrXORMappedAddress, stunAttrXORMappedAddrAlt} {
		if v, ok := m.attr(t); ok {
			if ap, err := stunDecodeAddr(v, true, m.TxID); err == nil {
				return ap, true
			}
		}
	}
	if v, ok := m.attr(stunAttrMappedAddress); ok {
		if ap, err := stunDecodeAddr(v, false, m.TxID); err == nil {
			return ap, true
		}
	}
	return netip.AddrPort{}, false
}

// otherAddr returns the alternate transport address the server advertises:
// OTHER-ADDRESS (RFC 5780) or, for older servers, CHANGED-ADDRESS (RFC 3489).
func (m *stunMessage) otherAddr() (netip.AddrPort, bool) {
	for _, t := range []uint16{stunAttrOtherAddress, stunAttrChangedAddress} {
		if v, ok := m.attr(t); ok {
			if ap, err := stunDecodeAddr(v, false, m.TxID); err == nil && ap.IsValid() {
				return ap, true
			}
		}
	}
	return netip.AddrPort{}, false
}

// software returns the SOFTWARE attribute, trimmed, or "".
func (m *stunMessage) software() string {
	v, ok := m.attr(stunAttrSoftware)
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(v), ""))
}

// errorCode decodes ERROR-CODE from an error response.
func (m *stunMessage) errorCode() (int, string, bool) {
	v, ok := m.attr(stunAttrErrorCode)
	if !ok || len(v) < 4 {
		return 0, "", false
	}
	code := int(v[2]&0x07)*100 + int(v[3])
	return code, strings.TrimSpace(strings.ToValidUTF8(string(v[4:]), "")), true
}

// stunDecodeAddr decodes an address attribute payload:
// reserved byte, family, port, address. When xor is set the port is XORed with
// the high half of the magic cookie and the address with the cookie (IPv4) or
// the cookie followed by the transaction ID (IPv6).
func stunDecodeAddr(v []byte, xor bool, txid [12]byte) (netip.AddrPort, error) {
	if len(v) < 4 {
		return netip.AddrPort{}, errSTUNTruncated
	}
	var n int
	switch v[1] {
	case stunFamilyV4:
		n = 4
	case stunFamilyV6:
		n = 16
	default:
		return netip.AddrPort{}, errSTUNFamily
	}
	if len(v) < 4+n {
		return netip.AddrPort{}, errSTUNTruncated
	}
	port := binary.BigEndian.Uint16(v[2:4])
	raw := make([]byte, n)
	copy(raw, v[4:4+n])
	if xor {
		port ^= uint16(stunMagicCookie >> 16)
		var mask [16]byte
		binary.BigEndian.PutUint32(mask[0:4], stunMagicCookie)
		copy(mask[4:], txid[:])
		for i := range raw {
			raw[i] ^= mask[i]
		}
	}
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.AddrPort{}, errSTUNFamily
	}
	return netip.AddrPortFrom(addr.Unmap(), port), nil
}

// stunEncodeAddr is the inverse of [stunDecodeAddr].
func stunEncodeAddr(ap netip.AddrPort, xor bool, txid [12]byte) []byte {
	addr := ap.Addr().Unmap()
	raw := addr.AsSlice()
	fam := stunFamilyV6
	if addr.Is4() {
		fam = stunFamilyV4
	}
	port := ap.Port()
	if xor {
		port ^= uint16(stunMagicCookie >> 16)
		var mask [16]byte
		binary.BigEndian.PutUint32(mask[0:4], stunMagicCookie)
		copy(mask[4:], txid[:])
		for i := range raw {
			raw[i] ^= mask[i]
		}
	}
	out := make([]byte, 4, 4+len(raw))
	out[1] = fam
	binary.BigEndian.PutUint16(out[2:4], port)
	return append(out, raw...)
}

// stunBindingRequestMsg builds a binding request with a fresh transaction ID,
// optionally carrying a CHANGE-REQUEST attribute built from the given flags.
func stunBindingRequestMsg(change byte) *stunMessage {
	m := &stunMessage{Type: stunBindingRequest}
	// crypto/rand.Read is documented never to fail.
	_, _ = rand.Read(m.TxID[:])
	if change != 0 {
		v := make([]byte, 4)
		v[3] = change
		m.Attrs = append(m.Attrs, stunAttr{Type: stunAttrChangeRequest, Value: v})
	}
	return m
}

// stunResponseFor parses raw and reports whether it is a binding response
// belonging to txid. Anything else — garbage, a stray packet, or a response to
// a different transaction — is rejected so the transactor keeps waiting.
func stunResponseFor(raw []byte, txid [12]byte) (*stunMessage, bool) {
	msg, err := parseSTUNMessage(raw)
	if err != nil {
		return nil, false
	}
	if msg.TxID != txid {
		return nil, false
	}
	if msg.Type != stunBindingSuccess && msg.Type != stunBindingError {
		return nil, false
	}
	return msg, true
}

// ---------------------------------------------------------------------------
// transport
// ---------------------------------------------------------------------------

// stunLog falls back to the default logger when the caller passed nil.
func stunLog(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// stunListen binds an unconnected wildcard UDP socket of the requested family.
func stunListen(v4 bool) (*net.UDPConn, error) {
	if v4 {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	}
	return net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: 0})
}

func stunUnmap(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// stunTransact sends req on conn and waits for the matching response,
// retransmitting up to attempts times roughly interval apart. Read deadlines
// are clamped to the context deadline and the context also aborts an in-flight
// read, so this can never outlive its caller.
func stunTransact(ctx context.Context, conn *net.UDPConn, dst netip.AddrPort, req *stunMessage, attempts int, interval time.Duration) (*stunMessage, netip.AddrPort, time.Duration, error) {
	if attempts <= 0 {
		attempts = 1
	}
	if interval <= 0 {
		interval = stunInterval
	}
	raw := req.encode()
	buf := make([]byte, stunMaxMessage)

	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return nil, netip.AddrPort{}, 0, err
		}
		sent := time.Now()
		if _, err := conn.WriteToUDPAddrPort(raw, dst); err != nil {
			return nil, netip.AddrPort{}, 0, err
		}
		deadline := sent.Add(interval)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		for time.Now().Before(deadline) {
			if err := conn.SetReadDeadline(deadline); err != nil {
				return nil, netip.AddrPort{}, 0, err
			}
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					break
				}
				return nil, netip.AddrPort{}, 0, err
			}
			msg, ok := stunResponseFor(buf[:n], req.TxID)
			if !ok {
				continue // stray packet or foreign transaction id
			}
			return msg, stunUnmap(from), time.Since(sent), nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, netip.AddrPort{}, 0, err
	}
	return nil, netip.AddrPort{}, 0, errSTUNTimeout
}

// stunQuery runs one transaction against dst on a throwaway socket of the
// destination's own family.
func stunQuery(ctx context.Context, dst netip.AddrPort, change byte, attempts int, interval time.Duration) (*stunMessage, netip.AddrPort, time.Duration, error) {
	conn, err := stunListen(dst.Addr().Is4())
	if err != nil {
		return nil, netip.AddrPort{}, 0, err
	}
	defer conn.Close()
	return stunTransact(ctx, conn, dst, stunBindingRequestMsg(change), attempts, interval)
}

// stunResolve splits a "host:port" target and resolves the host to IP
// addresses, IPv4 first. Both families are returned so a dead AAAA record can
// never mask a working A record, and so callers can report which family worked.
func stunResolve(ctx context.Context, hostport string) ([]netip.Addr, uint16, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, 0, err
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return nil, 0, fmt.Errorf("stun: invalid port %q", portStr)
	}
	port := uint16(portNum)
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}, port, nil
	}
	rctx, cancel := context.WithTimeout(ctx, stunResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		return nil, port, err
	}
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	// Sort so repeated runs probe the same address despite DNS round-robin.
	sort.Slice(v4, func(i, j int) bool { return v4[i].Compare(v4[j]) < 0 })
	sort.Slice(v6, func(i, j int) bool { return v6[i].Compare(v6[j]) < 0 })
	out := append(v4, v6...)
	if len(out) == 0 {
		return nil, port, errSTUNNoAddr
	}
	return out, port, nil
}

// stunHasLocalIP reports whether a is bound to one of this machine's
// interfaces, i.e. the reflexive address is not translated at all.
func stunHasLocalIP(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, ia := range addrs {
		var ip net.IP
		switch v := ia.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if got, ok := netip.AddrFromSlice(ip); ok && got.Unmap() == a {
			return true
		}
	}
	return false
}

func sortAddrPorts(s []netip.AddrPort) {
	sort.Slice(s, func(i, j int) bool {
		if c := s[i].Addr().Compare(s[j].Addr()); c != 0 {
			return c < 0
		}
		return s[i].Port() < s[j].Port()
	})
}

// ---------------------------------------------------------------------------
// ProbeSTUN
// ---------------------------------------------------------------------------

// ProbeSTUN runs one binding transaction against each server, in parallel.
//
// Results are returned in the same order as servers so the UI does not jitter
// between refreshes. An empty servers list falls back to [DefaultSTUNServers].
func ProbeSTUN(ctx context.Context, servers []STUNServer, logger *slog.Logger) []STUNResult {
	log := stunLog(logger).With(slog.String("from", "netdiag/stun"))
	if len(servers) == 0 {
		servers = DefaultSTUNServers()
	}
	out := make([]STUNResult, len(servers))
	sem := make(chan struct{}, stunMaxInFlight)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := STUNResult{Server: srv.Host, Name: srv.Name, Region: srv.Region}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				res.Err = ctx.Err().Error()
				mu.Lock()
				out[i] = res
				mu.Unlock()
				return
			}
			res = stunProbeServer(ctx, srv, log)
			mu.Lock()
			out[i] = res
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// stunProbeServer probes one server, trying every resolved address (IPv4 first)
// until one answers.
func stunProbeServer(ctx context.Context, srv STUNServer, log *slog.Logger) STUNResult {
	res := STUNResult{Server: srv.Host, Name: srv.Name, Region: srv.Region}
	addrs, port, err := stunResolve(ctx, srv.Host)
	if err != nil {
		res.Err = err.Error()
		log.With(slog.String("server", srv.Host), slog.String("error", err.Error())).Debug("stun resolve failed")
		return res
	}
	for _, a := range addrs {
		dst := netip.AddrPortFrom(a, port)
		pctx, cancel := context.WithTimeout(ctx, stunProbeTimeout)
		msg, _, rtt, err := stunQuery(pctx, dst, 0, stunAttempts, stunInterval)
		cancel()
		if err != nil {
			res.Err = err.Error()
			continue
		}
		if code, reason, ok := msg.errorCode(); ok {
			res.Err = fmt.Sprintf("stun error %d %s", code, reason)
			continue
		}
		mapped, hasMapped := msg.mappedAddr()
		if !hasMapped {
			res.Err = "stun: response without mapped address"
			continue
		}
		res.OK = true
		res.Err = ""
		res.RTT = rtt
		res.Mapped = mapped
		res.Software = msg.software()
		if other, ok := msg.otherAddr(); ok {
			res.Other = other
			// Only worth asking for a CHANGE-REQUEST when the server has an
			// alternate address to answer from; a silent drop otherwise costs a
			// full timeout and tells us nothing.
			cctx, ccancel := context.WithTimeout(ctx, stunQuickInterval*time.Duration(stunQuickAttempts+1))
			crMsg, crFrom, _, crErr := stunQuery(cctx, dst, stunChangeIP|stunChangePort, stunQuickAttempts, stunQuickInterval)
			ccancel()
			res.SupportsChangeReq = crErr == nil && crMsg != nil && crFrom != dst
		}
		log.With(
			slog.String("server", srv.Host),
			slog.String("mapped", mapped.String()),
			slog.Duration("rtt", rtt),
		).Debug("stun binding ok")
		break
	}
	return res
}

// ---------------------------------------------------------------------------
// ProbeUDP
// ---------------------------------------------------------------------------

// udpAttempt pairs a probe with the address family it used; the contract's
// UDPProbe has no family field, so it is tracked alongside.
type udpAttempt struct {
	probe UDPProbe
	v6    bool
	// resolveFailed marks a probe that never reached the send stage because
	// the hostname would not resolve. Its port must not count towards the
	// blocked-port analysis: a failed A-record lookup says nothing about
	// whether UDP on that port can leave the machine.
	resolveFailed bool
}

// ProbeUDP reports whether UDP can leave the machine at all, over v4 and v6,
// and on which destination ports.
//
// Each server is resolved to both A and AAAA records and probed once per
// family, so a broken IPv6 path is visible instead of being hidden behind a
// working IPv4 one. Probes run in parallel; the report is ordered by the input
// server list.
func ProbeUDP(ctx context.Context, servers []STUNServer, logger *slog.Logger) UDPReport {
	log := stunLog(logger).With(slog.String("from", "netdiag/udp"))
	if len(servers) == 0 {
		servers = DefaultSTUNServers()
	}
	var rep UDPReport

	per := make([][]udpAttempt, len(servers))
	sem := make(chan struct{}, stunMaxInFlight)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				// Record the cancellation instead of dropping the server:
				// leaving per[i] nil would still count it towards the
				// CN/international totals while producing no evidence row, so
				// the report would claim servers were unreachable that were
				// never contacted.
				mu.Lock()
				per[i] = []udpAttempt{{
					probe: UDPProbe{
						Host:   srv.Host,
						Target: srv.Host,
						Name:   srv.Name,
						Region: srv.Region,
						Err:    ctx.Err().Error(),
					},
					resolveFailed: true,
				}}
				mu.Unlock()
				return
			}
			got := stunProbeUDPServer(ctx, srv, log)
			mu.Lock()
			per[i] = got
			mu.Unlock()
		}()
	}
	wg.Wait()

	portOK := map[int]bool{}
	portSeen := map[int]bool{}
	for i, srv := range servers {
		serverOK := false
		for _, at := range per[i] {
			rep.Probes = append(rep.Probes, at.probe)
			if at.probe.Port > 0 && !at.resolveFailed {
				portSeen[at.probe.Port] = true
			}
			if !at.probe.OK {
				continue
			}
			serverOK = true
			if at.probe.Port > 0 {
				portOK[at.probe.Port] = true
			}
			if at.v6 {
				rep.V6OK = true
			} else {
				rep.V4OK = true
			}
		}
		switch srv.Region {
		case RegionCN:
			rep.CNTotal++
			if serverOK {
				rep.CNReachable++
			}
		default:
			rep.IntlTotal++
			if serverOK {
				rep.IntlReachabl++
			}
		}
	}

	for p := range portSeen {
		if portOK[p] {
			rep.OKPorts = append(rep.OKPorts, p)
		}
	}
	// A port only counts as blocked when some other port worked; otherwise UDP
	// as a whole is down and singling out ports would be misleading.
	if len(rep.OKPorts) > 0 {
		for p := range portSeen {
			if !portOK[p] {
				rep.BlockedPorts = append(rep.BlockedPorts, p)
			}
		}
	}
	sort.Ints(rep.OKPorts)
	sort.Ints(rep.BlockedPorts)

	rep.Status, rep.Summary = stunUDPVerdict(&rep)
	log.With(
		slog.Bool("v4", rep.V4OK),
		slog.Bool("v6", rep.V6OK),
		slog.Int("cn", rep.CNReachable),
		slog.Int("intl", rep.IntlReachabl),
	).Info("udp reachability probed")
	return rep
}

// stunProbeUDPServer probes one server once per resolved address family.
func stunProbeUDPServer(ctx context.Context, srv STUNServer, log *slog.Logger) []udpAttempt {
	addrs, port, err := stunResolve(ctx, srv.Host)
	if err != nil {
		return []udpAttempt{{
			probe: UDPProbe{
				Host:   srv.Host,
				Target: srv.Host,
				Name:   srv.Name,
				Region: srv.Region,
				Port:   int(port),
				Err:    err.Error(),
			},
			resolveFailed: true,
		}}
	}
	var out []udpAttempt
	var doneV4, doneV6 bool
	for _, a := range addrs {
		v6 := !a.Is4()
		if (v6 && doneV6) || (!v6 && doneV4) {
			continue
		}
		if v6 {
			doneV6 = true
		} else {
			doneV4 = true
		}
		dst := netip.AddrPortFrom(a, port)
		p := UDPProbe{Host: srv.Host, Target: dst.String(), Name: srv.Name, Region: srv.Region, Port: int(port)}
		pctx, cancel := context.WithTimeout(ctx, stunProbeTimeout)
		msg, _, rtt, err := stunQuery(pctx, dst, 0, stunAttempts, stunInterval)
		cancel()
		switch {
		case err != nil:
			p.Err = err.Error()
		default:
			p.OK = true
			p.RTT = rtt
			if m, ok := msg.mappedAddr(); ok {
				p.Mapped = m
			}
		}
		out = append(out, udpAttempt{probe: p, v6: v6})
	}
	return out
}

// stunUDPVerdict turns the counters into a traffic light and one Chinese line.
func stunUDPVerdict(rep *UDPReport) (Status, string) {
	if !rep.V4OK && !rep.V6OK {
		return StatusFail, fmt.Sprintf("UDP 完全不通：%d 次探测全部失败，P2P 打洞不可用，只能走 TCP/DERP 中继", len(rep.Probes))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "UDP 可用（IPv4 %s，IPv6 %s）；境内 %d/%d，境外 %d/%d",
		stunOKText(rep.V4OK), stunOKText(rep.V6OK),
		rep.CNReachable, rep.CNTotal, rep.IntlReachabl, rep.IntlTotal)

	status := StatusOK
	if len(rep.BlockedPorts) > 0 {
		parts := make([]string, 0, len(rep.BlockedPorts))
		for _, p := range rep.BlockedPorts {
			parts = append(parts, strconv.Itoa(p))
		}
		fmt.Fprintf(&b, "；端口 %s 疑似被封锁", strings.Join(parts, "/"))
		status = StatusWarn
	}
	if !rep.V4OK {
		b.WriteString("；IPv4 UDP 不通，多数对端将无法直连")
		status = StatusWarn
	}
	if rep.IntlTotal > 0 && rep.IntlReachabl == 0 {
		b.WriteString("；境外 STUN 全部不可达，出境 UDP 可能被拦截")
		status = StatusWarn
	}
	return status, b.String()
}

func stunOKText(ok bool) string {
	if ok {
		return "通"
	}
	return "不通"
}

// ---------------------------------------------------------------------------
// ClassifyNAT
// ---------------------------------------------------------------------------

// stunTarget is one resolved probe endpoint.
type stunTarget struct {
	srv STUNServer
	dst netip.AddrPort
}

// natClassifier holds the state of one classification run. Every mapping test
// reuses the same socket, because the mapping is a property of that socket.
type natClassifier struct {
	ctx       context.Context
	log       *slog.Logger
	conn      *net.UDPConn
	localPort uint16
	rep       *NATReport
	seen      map[netip.AddrPort]bool
}

// ClassifyNAT performs RFC 5780 behaviour discovery and maps the result onto
// the classic RFC 3489 NAT names.
//
// The whole run is bounded by the caller's context and by an internal budget,
// and every individual failure degrades into a note instead of aborting: a
// partial classification still renders.
func ClassifyNAT(ctx context.Context, servers []STUNServer, logger *slog.Logger) NATReport {
	log := stunLog(logger).With(slog.String("from", "netdiag/nat"))
	if len(servers) == 0 {
		servers = DefaultSTUNServers()
	}
	rep := NATReport{Type: NATUnknown, Status: StatusUnknown}

	ctx, cancel := context.WithTimeout(ctx, natTotalBudget)
	defer cancel()

	// 1. one socket for every mapping test.
	conn, err := stunListen(true)
	if err != nil {
		rep.Status = StatusFail
		rep.Summary = "无法创建 UDP 套接字，NAT 检测中止：" + err.Error()
		rep.Notes = append(rep.Notes, err.Error())
		return rep
	}
	defer conn.Close()

	c := &natClassifier{ctx: ctx, log: log, conn: conn, rep: &rep, seen: map[netip.AddrPort]bool{}}
	if la, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		c.localPort = uint16(la.Port)
	}

	targets := stunResolveTargets(ctx, servers, log)
	if len(targets) == 0 {
		rep.Status = StatusFail
		rep.Summary = "没有可用的 STUN 服务器地址（DNS 解析全部失败）"
		rep.Notes = append(rep.Notes, "所有 STUN 服务器的 IPv4 地址解析失败")
		return rep
	}

	// 2. test I: first server that answers at all.
	primary, firstMsg, ok := c.firstAnswer(targets, nil)
	if !ok {
		rep.Type = NATUDPBlocked
		rep.Status = StatusFail
		rep.Summary = "UDP 出站被完全阻断：所有 STUN 服务器均无响应，P2P 打洞不可用，连接将全程回退到 DERP 中继"
		rep.Notes = append(rep.Notes, "检查防火墙是否放行 UDP 出站，或运营商是否封锁 UDP")
		rep.MappedAddrs = c.mappedList()
		log.Warn("no stun server answered, udp appears blocked")
		return rep
	}
	firstMapped, _ := firstMsg.mappedAddr()
	rep.PortPreserving = boolPtr(firstMapped.Port() == c.localPort)

	// 3. is there a NAT at all?
	noNAT := firstMapped.Port() == c.localPort && stunHasLocalIP(firstMapped.Addr())
	if noNAT {
		rep.Mapping = BehaviorEndpointIndependent
		rep.Notes = append(rep.Notes, "反射地址与本机地址一致，链路上没有 NAT")
	} else {
		// 4. mapping behaviour, from the same socket to a different server IP.
		rep.Mapping = c.mappingBehavior(targets, primary, firstMsg, firstMapped)
	}

	// 5. filtering behaviour; needs a real RFC 5780 server.
	rep.Filtering = c.filteringBehavior()
	if rep.Filtering == BehaviorUnknown {
		rep.Notes = append(rep.Notes,
			"没有 STUN 服务器响应 CHANGE-REQUEST（Google/Cloudflare 等只支持基本绑定请求），无法判定过滤行为")
	}

	// 6. legacy name.
	rep.Type = stunLegacyNATType(noNAT, rep.Mapping, rep.Filtering)
	if rep.Type == NATUnknown && rep.Mapping == BehaviorEndpointIndependent {
		rep.Notes = append(rep.Notes,
			"映射行为为端点无关，但过滤行为未知，无法安全地断言为端口限制锥型 NAT")
	}

	// 7. hairpinning, best effort. Left nil when the test could not run.
	if firstMapped.IsValid() {
		if got, ok := c.hairpin(firstMapped); ok {
			rep.Hairpin = boolPtr(got)
		} else {
			rep.Notes = append(rep.Notes, "发夹回环测试未能执行（时间预算已用尽或无法创建探测套接字）")
		}
	}

	// 9. distinct reflexive addresses, sorted.
	rep.MappedAddrs = c.mappedList()
	rep.Status, rep.Summary = stunNATVerdict(rep.Type, rep.Mapping, rep.Filtering)

	log.With(
		slog.String("type", string(rep.Type)),
		slog.String("mapping", rep.Mapping.String()),
		slog.String("filtering", rep.Filtering.String()),
		slog.Int("mapped", len(rep.MappedAddrs)),
	).Info("nat classified")
	return rep
}

// stunResolveTargets resolves each server to a single IPv4 endpoint, in
// parallel. IPv6 is skipped here: the classifier binds one IPv4 socket so the
// mapping under test is well defined.
func stunResolveTargets(ctx context.Context, servers []STUNServer, log *slog.Logger) []stunTarget {
	out := make([]stunTarget, len(servers))
	found := make([]bool, len(servers))
	sem := make(chan struct{}, stunMaxInFlight)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			addrs, port, err := stunResolve(ctx, srv.Host)
			if err != nil {
				log.With(slog.String("server", srv.Host), slog.String("error", err.Error())).Debug("resolve failed")
				return
			}
			for _, a := range addrs {
				if !a.Is4() {
					continue
				}
				mu.Lock()
				out[i] = stunTarget{srv: srv, dst: netip.AddrPortFrom(a, port)}
				found[i] = true
				mu.Unlock()
				return
			}
		}()
	}
	wg.Wait()

	res := make([]stunTarget, 0, len(servers))
	for i := range out {
		if found[i] {
			res = append(res, out[i])
		}
	}
	return res
}

// query runs one transaction on the shared socket and records a STUNResult, so
// even failed steps show up in the report.
func (c *natClassifier) query(t stunTarget, change byte, attempts int, interval time.Duration) (*stunMessage, netip.AddrPort, bool) {
	res := STUNResult{Server: t.dst.String(), Name: t.srv.Name, Region: t.srv.Region}
	msg, from, rtt, err := stunTransact(c.ctx, c.conn, t.dst, stunBindingRequestMsg(change), attempts, interval)
	if err != nil {
		res.Err = err.Error()
		c.rep.Results = append(c.rep.Results, res)
		return nil, netip.AddrPort{}, false
	}
	res.RTT = rtt
	res.Software = msg.software()
	if code, reason, ok := msg.errorCode(); ok {
		res.Err = fmt.Sprintf("stun error %d %s", code, reason)
		c.rep.Results = append(c.rep.Results, res)
		return nil, from, false
	}
	res.OK = true
	if m, ok := msg.mappedAddr(); ok {
		res.Mapped = m
		c.seen[m] = true
	}
	if o, ok := msg.otherAddr(); ok {
		res.Other = o
	}
	// A CHANGE-REQUEST only counts as honoured when the answer really came back
	// from another transport address.
	res.SupportsChangeReq = change != 0 && from != t.dst
	c.rep.Results = append(c.rep.Results, res)
	return msg, from, true
}

// firstAnswer queries targets in order, skipping any whose IP is in skip, and
// returns the first one that answers with a mapped address.
func (c *natClassifier) firstAnswer(targets []stunTarget, skip []netip.Addr) (stunTarget, *stunMessage, bool) {
	for _, t := range targets {
		if c.ctx.Err() != nil {
			break
		}
		skipped := false
		for _, s := range skip {
			if t.dst.Addr() == s {
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}
		msg, _, ok := c.query(t, 0, stunAttempts, stunInterval)
		if !ok {
			continue
		}
		if _, has := msg.mappedAddr(); !has {
			continue
		}
		return t, msg, true
	}
	return stunTarget{}, nil, false
}

// mappingBehavior implements RFC 5780 §4.3 over the shared socket.
//
// The three tests must vary exactly one thing at a time:
//
//	Test I   primaryIP:primaryPort   -> mapped1
//	Test II  alternateIP:primaryPort -> mapped2   (destination IP changed)
//	Test III alternateIP:alternatePort -> mapped3 (destination port changed)
//
// mapped2 == mapped1 means endpoint-independent. Otherwise mapped3 == mapped2
// means address-dependent, and a third distinct mapping means
// address-and-port-dependent.
//
// That sequence needs one server advertising an OTHER-ADDRESS, because an
// RFC 5780 server listens on all four combinations of its primary/alternate
// address and port. Using two *different* servers for tests II and III would
// change the IP and the port at once, which makes address-dependent
// unreachable — every address-dependent NAT would be reported as
// address-and-port-dependent.
func (c *natClassifier) mappingBehavior(targets []stunTarget, primary stunTarget, firstMsg *stunMessage, firstMapped netip.AddrPort) Behavior {
	// Preferred: run the full sequence against a server that advertises an
	// alternate transport address. Try the primary first, then the others.
	if b, ok := c.mappingViaAlternate(primary, firstMsg, firstMapped); ok {
		return b
	}
	for _, t := range targets {
		if c.ctx.Err() != nil {
			break
		}
		if t.dst == primary.dst {
			continue
		}
		msg, _, ok := c.query(t, 0, stunQuickAttempts, stunInterval)
		if !ok {
			continue
		}
		mapped, has := msg.mappedAddr()
		if !has {
			continue
		}
		if b, ok := c.mappingViaAlternate(t, msg, mapped); ok {
			return b
		}
	}

	// No server offered an alternate address. A second server still separates
	// endpoint-independent from the rest, which is the distinction that
	// actually decides whether hole punching can work.
	second, secondMsg, ok := c.firstAnswer(targets, []netip.Addr{primary.dst.Addr()})
	if !ok {
		c.rep.Notes = append(c.rep.Notes, "只有一台 STUN 服务器可达，无法判定映射行为")
		return BehaviorUnknown
	}
	secondMapped, _ := secondMsg.mappedAddr()
	if secondMapped == firstMapped {
		return BehaviorEndpointIndependent
	}
	_ = second
	c.rep.Notes = append(c.rep.Notes,
		"映射随目标地址变化，但没有 STUN 服务器提供备用端口，无法区分地址相关与地址端口相关映射")
	return BehaviorUnknown
}

// mappingViaAlternate runs RFC 5780 tests II and III against one server's
// alternate transport address. It reports ok=false when the server advertises
// no usable OTHER-ADDRESS or stops answering, so the caller can try another
// server rather than record a guess.
func (c *natClassifier) mappingViaAlternate(base stunTarget, baseMsg *stunMessage, baseMapped netip.AddrPort) (Behavior, bool) {
	alt, has := baseMsg.otherAddr()
	if !has || !alt.Addr().Is4() {
		return BehaviorUnknown, false
	}
	// Both coordinates must actually differ, otherwise there is no second
	// dimension to test.
	if alt.Addr() == base.dst.Addr() || alt.Port() == base.dst.Port() {
		return BehaviorUnknown, false
	}

	name := func(suffix string) STUNServer {
		srv := base.srv
		srv.Name = srv.Name + suffix
		return srv
	}

	// Test II: alternate IP, same port. Only the destination IP changed.
	t2 := stunTarget{
		srv: name("(备用地址)"),
		dst: netip.AddrPortFrom(alt.Addr(), base.dst.Port()),
	}
	msg2, _, ok := c.query(t2, 0, stunQuickAttempts, stunInterval)
	if !ok {
		return BehaviorUnknown, false
	}
	mapped2, has := msg2.mappedAddr()
	if !has {
		return BehaviorUnknown, false
	}
	if mapped2 == baseMapped {
		return BehaviorEndpointIndependent, true
	}

	// Test III: same alternate IP, alternate port. Only the port changed
	// relative to test II.
	t3 := stunTarget{srv: name("(备用地址+端口)"), dst: alt}
	msg3, _, ok := c.query(t3, 0, stunQuickAttempts, stunInterval)
	if !ok {
		return BehaviorUnknown, false
	}
	mapped3, has := msg3.mappedAddr()
	if !has {
		return BehaviorUnknown, false
	}
	if mapped3 == mapped2 {
		return BehaviorAddressDependent, true
	}
	return BehaviorAddressAndPortDependent, true
}

// filteringBehavior runs RFC 5780 tests II and III (CHANGE-REQUEST) against the
// first server that both answers a plain binding request and advertises
// OTHER-ADDRESS. Servers that ignore CHANGE-REQUEST are skipped rather than
// interpreted, because a silent drop is indistinguishable from filtering.
func (c *natClassifier) filteringBehavior() Behavior {
	var (
		ignoring []string // servers that answered but ignored CHANGE-REQUEST
		silent   string   // first server that answered plain bindings but no change requests
	)
	for _, srv := range RFC5780Servers() {
		if c.ctx.Err() != nil {
			break
		}
		addrs, port, err := stunResolve(c.ctx, srv.Host)
		if err != nil {
			continue
		}
		var dst netip.AddrPort
		for _, a := range addrs {
			if a.Is4() {
				dst = netip.AddrPortFrom(a, port)
				break
			}
		}
		if !dst.IsValid() {
			continue
		}
		t := stunTarget{srv: srv, dst: dst}
		msg, _, ok := c.query(t, 0, stunQuickAttempts, stunInterval)
		if !ok {
			continue
		}
		if _, has := msg.otherAddr(); !has {
			continue // no alternate address: cannot answer a CHANGE-REQUEST
		}

		// Test II: ask for a reply from another IP *and* port.
		//
		// Three outcomes have to be told apart. A reply from a different
		// transport address proves the server honoured the request and that
		// nothing filtered it. A reply from the address we asked proves the
		// server ignored the attribute, which says nothing about filtering —
		// treating it as "filtered" is how a full-cone NAT ends up reported as
		// port-restricted. No reply at all is only meaningful once we know the
		// server honours CHANGE-REQUEST.
		_, from, ok := c.query(t, stunChangeIP|stunChangePort, stunQuickAttempts, stunQuickInterval)
		if ok && from != dst {
			return BehaviorEndpointIndependent
		}
		if ok {
			ignoring = append(ignoring, srv.Host)
			continue
		}

		// Test III: another port on the same IP. A reply here also proves the
		// server implements CHANGE-REQUEST, which retroactively makes the
		// silence in test II real evidence of address-dependent filtering.
		_, from, ok = c.query(t, stunChangePort, stunQuickAttempts, stunQuickInterval)
		if ok && from != dst {
			return BehaviorAddressDependent
		}
		if ok {
			ignoring = append(ignoring, srv.Host)
			continue
		}

		// Silence on both. This is what a port-restricted NAT looks like, but
		// it is also what a server that silently drops CHANGE-REQUEST looks
		// like. Remember it and keep looking for a server that demonstrably
		// honours the attribute; only fall back to this if none does.
		if silent == "" {
			silent = srv.Host
		}
	}

	if silent != "" {
		c.rep.Notes = append(c.rep.Notes, fmt.Sprintf(
			"过滤行为依据 %s 对 CHANGE-REQUEST 的静默推断：该服务器通告了备用地址且能回应普通绑定请求，"+
				"但两次改址请求均无回应，最可能是本地 NAT 拦截", silent))
		return BehaviorAddressAndPortDependent
	}
	if len(ignoring) > 0 {
		c.rep.Notes = append(c.rep.Notes, fmt.Sprintf(
			"%s 忽略了 CHANGE-REQUEST（仍从原地址回包），无法判定过滤行为",
			strings.Join(ignoring, "、")))
	} else {
		c.rep.Notes = append(c.rep.Notes,
			"没有可用的 RFC 5780 服务器，无法判定过滤行为")
	}
	return BehaviorUnknown
}

// hairpin sends a binding request to our own reflexive address from a second
// socket and checks whether the first socket sees it. Best effort: any failure
// simply means "no hairpinning observed" and never fails the classification.
// It returns ok=false when the test could not be performed at all, so the
// caller can leave NATReport.Hairpin nil. That distinction matters: this test
// runs last, after up to 20s of serial STUN transactions, so an exhausted
// budget is the common case — and reporting "hairpinning not supported" for a
// probe that never sent a packet is a wrong answer, not a cautious one.
func (c *natClassifier) hairpin(mapped netip.AddrPort) (result bool, ok bool) {
	if !mapped.IsValid() || !mapped.Addr().Is4() {
		return false, false
	}
	deadline := time.Now().Add(stunHairpinTimeout)
	if d, dok := c.ctx.Deadline(); dok && d.Before(deadline) {
		deadline = d
	}
	if !time.Now().Before(deadline) {
		return false, false // no budget left to send anything
	}

	probe, err := stunListen(true)
	if err != nil {
		return false, false
	}
	defer probe.Close()

	req := stunBindingRequestMsg(0)
	if _, err := probe.WriteToUDPAddrPort(req.encode(), mapped); err != nil {
		return false, false
	}

	// From here on the probe was actually sent, so silence is a real answer.
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, stunMaxMessage)
	for time.Now().Before(deadline) {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return false, true
		}
		n, _, err := c.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return false, true
		}
		msg, err := parseSTUNMessage(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type == stunBindingRequest && msg.TxID == req.TxID {
			return true, true
		}
	}
	return false, true
}

// mappedList returns every distinct reflexive address observed, sorted.
func (c *natClassifier) mappedList() []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(c.seen))
	for ap := range c.seen {
		out = append(out, ap)
	}
	sortAddrPorts(out)
	return out
}

// stunLegacyNATType maps RFC 5780 behaviours onto the RFC 3489 names users
// recognise. Unknown filtering never gets guessed away.
func stunLegacyNATType(noNAT bool, mapping, filtering Behavior) NATType {
	switch {
	case mapping == BehaviorAddressDependent || mapping == BehaviorAddressAndPortDependent:
		return NATSymmetric
	case noNAT:
		switch filtering {
		case BehaviorEndpointIndependent:
			return NATOpen
		case BehaviorUnknown:
			return NATUnknown
		default:
			return NATSymmetricFW
		}
	case mapping == BehaviorEndpointIndependent:
		switch filtering {
		case BehaviorEndpointIndependent:
			return NATFullCone
		case BehaviorAddressDependent:
			return NATRestricted
		case BehaviorAddressAndPortDependent:
			return NATPortRestrict
		default:
			return NATUnknown
		}
	default:
		return NATUnknown
	}
}

// stunNATVerdict turns the NAT type into a traffic light plus a one-line
// Chinese explanation of what it means for P2P.
func stunNATVerdict(t NATType, mapping, filtering Behavior) (Status, string) {
	switch t {
	case NATOpen:
		return StatusOK, "公网直连，没有 NAT，P2P 打洞不受限制"
	case NATFullCone:
		return StatusOK, "全锥型 NAT，打洞成功率很高，通常可以直连"
	case NATRestricted:
		return StatusOK, "地址限制锥型 NAT，双方同时发包即可打洞，直连通常成功"
	case NATPortRestrict:
		return StatusWarn, "端口限制锥型 NAT，多数情况下能打洞成功，偶尔会回退到 DERP 中继"
	case NATSymmetric:
		return StatusFail, "对称型 NAT 会导致打洞失败，连接将回退到 DERP 中继，延迟和带宽都会变差"
	case NATUDPBlocked:
		return StatusFail, "UDP 被阻断，无法打洞，连接将全程走 DERP 中继"
	case NATSymmetricFW:
		return StatusWarn, "没有 NAT 但存在有状态防火墙，需要本机先发包，对端才能回连"
	default:
		return StatusWarn, fmt.Sprintf("无法确定 NAT 类型（映射行为：%s，过滤行为：%s），打洞结果不可预测", mapping, filtering)
	}
}
