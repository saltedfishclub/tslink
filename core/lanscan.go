package core

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Minecraft's LAN discovery protocol: servers multicast the ASCII payload
// "[MOTD]<motd>[/MOTD][AD]<port>[/AD]" to these groups roughly every 1.5s.
// core/lan.go sends them; this file listens for them.
const (
	lanScanGroupV4 = "224.0.2.60:4445"
	lanScanGroupV6 = "[ff75:230::60]:4445"
)

const (
	// lanScanExpiry drops a server that stopped broadcasting.
	lanScanExpiry = 30 * time.Second
	// lanScanStale is how long a server may go unheard before the next packet
	// from it is treated as a real change worth waking the UI for. Without it
	// the GUI would redraw on every duplicate broadcast.
	lanScanStale = 10 * time.Second
	// lanScanSweep is the expiry tick interval.
	lanScanSweep = 5 * time.Second
	// lanScanBuf is the per-read buffer size; LAN announcements are tiny.
	lanScanBuf = 2048
	// lanScanMotdRunes caps a stored MOTD so a hostile peer cannot bloat the UI.
	lanScanMotdRunes = 120
)

// LanServer is one Minecraft server seen broadcasting on the local network.
type LanServer struct {
	Motd      string         // MOTD with Minecraft section-sign colour codes stripped
	RawMotd   string         // as received
	Port      int            //
	Source    netip.AddrPort // who sent the packet
	Addr      netip.Addr     // Source.Addr(), the address to actually connect to
	FirstSeen time.Time
	LastSeen  time.Time
	Count     int  // packets seen
	IsSelf    bool // matches one of the entries tslink is advertising
}

// lanScanKey deduplicates by sender address and advertised port. The sender's
// ephemeral source port is deliberately excluded: it changes per socket.
type lanScanKey struct {
	addr netip.Addr
	port int
}

// LanScanner watches for Minecraft LAN broadcasts on every multicast-capable
// interface and keeps a deduplicated, self-expiring view of what it heard.
//
// All methods are safe for concurrent use; the GUI calls [LanScanner.Servers]
// from its frame loop while the read goroutines are writing.
type LanScanner struct {
	logger *slog.Logger

	mu      sync.RWMutex
	servers map[lanScanKey]*LanServer
	self    []LanEntry
	lastErr string
	subs    map[int]chan struct{}
	nextSub int

	started bool
	// live counts read loops still running. A VPN or virtual adapter going
	// down kills its socket's loop; when the last one dies the scanner is
	// deaf, and Err() has to say so instead of continuing to report health.
	live int
}

// NewLanScanner returns a scanner that has not started listening yet. A nil
// logger falls back to slog.Default.
func NewLanScanner(logger *slog.Logger) *LanScanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &LanScanner{
		logger:  logger.With(slog.String("from", "lanscan")),
		servers: make(map[lanScanKey]*LanServer),
		subs:    make(map[int]chan struct{}),
	}
}

// SetSelfEntries tells the scanner which advertisements are our own, so the UI
// can distinguish "the tunnel is working" from "someone else is hosting". It
// may be called after Start and re-evaluates already-known servers.
func (s *LanScanner) SetSelfEntries(entries []LanEntry) {
	cp := make([]LanEntry, len(entries))
	copy(cp, entries)

	s.mu.Lock()
	s.self = cp
	changed := false
	for _, srv := range s.servers {
		self := matchesSelf(cp, srv.RawMotd, srv.Port)
		if self != srv.IsSelf {
			srv.IsSelf = self
			changed = true
		}
	}
	if changed {
		s.notifyLocked()
	}
	s.mu.Unlock()
}

// Start begins listening; it returns immediately and stops when ctx is done.
// Calling it twice is a no-op.
func (s *LanScanner) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	conns := s.listen()
	if len(conns) == 0 {
		s.mu.Lock()
		s.lastErr = "no multicast listener could be created"
		// Clear the guard so a caller that notices Err() can retry once the
		// network stack is up. Binding can fail simply because Start ran
		// before the interfaces existed, and a permanently dead scanner is a
		// worse outcome than a redundant retry.
		s.started = false
		s.mu.Unlock()
		s.logger.Warn("lan scan disabled, all multicast binds failed")
		return
	}
	s.logger.With(slog.Int("sockets", len(conns))).Debug("lan scan listening")

	// One closer goroutine unblocks every read at once on cancellation.
	go func() {
		<-ctx.Done()
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	s.mu.Lock()
	s.live = len(conns)
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *net.UDPConn) {
			defer wg.Done()
			defer s.readerExited(ctx)
			s.readLoop(ctx, c)
		}(c)
	}
	go s.sweepLoop(ctx)
	go func() {
		wg.Wait()
		s.logger.Debug("lan scan stopped")
	}()
}

// listen joins the IPv4 group on every up, multicast-capable interface plus a
// nil-interface fallback, then does the same for IPv6. Per-interface failures
// are expected (containers, down VPN adapters) and only logged at debug level.
func (s *LanScanner) listen() []*net.UDPConn {
	var conns []*net.UDPConn

	v4, err := net.ResolveUDPAddr("udp4", lanScanGroupV4)
	if err != nil {
		s.logger.With(slog.String("error", err.Error())).Error("failed to resolve ipv4 multicast group")
	}
	v6, err := net.ResolveUDPAddr("udp6", lanScanGroupV6)
	if err != nil {
		s.logger.With(slog.String("error", err.Error())).Debug("failed to resolve ipv6 multicast group")
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		s.logger.With(slog.String("error", err.Error())).Warn("failed to enumerate interfaces, falling back to default")
		ifaces = nil
	}

	for i := range ifaces {
		ifi := ifaces[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if v4 != nil {
			if c, err := net.ListenMulticastUDP("udp4", &ifi, v4); err == nil {
				conns = append(conns, c)
			} else {
				s.logger.With(
					slog.String("iface", ifi.Name),
					slog.String("error", err.Error()),
				).Debug("ipv4 multicast join failed")
			}
		}
		if v6 != nil {
			if c, err := net.ListenMulticastUDP("udp6", &ifi, v6); err == nil {
				conns = append(conns, c)
			} else {
				s.logger.With(
					slog.String("iface", ifi.Name),
					slog.String("error", err.Error()),
				).Debug("ipv6 multicast join failed")
			}
		}
	}

	// Fallback: let the OS pick the interface. On some hosts this is the only
	// socket that ever receives anything.
	if v4 != nil {
		if c, err := net.ListenMulticastUDP("udp4", nil, v4); err == nil {
			conns = append(conns, c)
		} else {
			s.logger.With(slog.String("error", err.Error())).Debug("default ipv4 multicast join failed")
		}
	}
	if v6 != nil {
		if c, err := net.ListenMulticastUDP("udp6", nil, v6); err == nil {
			conns = append(conns, c)
		} else {
			s.logger.With(slog.String("error", err.Error())).Debug("default ipv6 multicast join failed")
		}
	}

	for _, c := range conns {
		_ = c.SetReadBuffer(64 * 1024)
	}
	return conns
}

// readLoop drains one socket until ctx is done or the socket is closed. A
// malformed packet is logged at debug level and never terminates the loop.
func (s *LanScanner) readLoop(ctx context.Context, c *net.UDPConn) {
	buf := make([]byte, lanScanBuf)
	for {
		if ctx.Err() != nil {
			return
		}
		// A deadline guarantees the loop notices cancellation even if the
		// closer goroutine has not run yet.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Anything else (ENETDOWN from an adapter disappearing, for
			// instance) means this socket is finished. Release it here rather
			// than leaving the fd until the process exits; the ctx closer
			// goroutine would otherwise be the only thing that ever closes it.
			s.logger.With(slog.String("error", err.Error())).Debug("lan scan read failed")
			_ = c.Close()
			return
		}
		if n <= 0 || src == nil {
			continue
		}
		ap, ok := netip.AddrFromSlice(src.IP)
		if !ok {
			continue
		}
		s.handle(netip.AddrPortFrom(ap.Unmap(), uint16(src.Port)), string(buf[:n]))
	}
}

// readerExited records that one read loop finished. Once every socket is gone
// while the scanner is still meant to be running, Err() must report it — the
// UI otherwise shows a healthy "listening" chip over a scanner that will never
// hear another packet.
func (s *LanScanner) readerExited(ctx context.Context) {
	s.mu.Lock()
	if s.live > 0 {
		s.live--
	}
	dead := s.live == 0 && ctx.Err() == nil
	if dead {
		s.lastErr = "all multicast listeners stopped, restart to rescan"
		s.started = false
		s.notifyLocked()
	}
	s.mu.Unlock()
	if dead {
		s.logger.Warn("lan scan has no live listeners left")
	}
}

// sweepLoop expires servers that stopped broadcasting.
func (s *LanScanner) sweepLoop(ctx context.Context) {
	t := time.NewTicker(lanScanSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.expire(time.Now())
		}
	}
}

func (s *LanScanner) expire(now time.Time) {
	s.mu.Lock()
	changed := false
	for k, srv := range s.servers {
		if now.Sub(srv.LastSeen) > lanScanExpiry {
			delete(s.servers, k)
			changed = true
			s.logger.With(
				slog.String("addr", srv.Addr.String()),
				slog.Int("port", srv.Port),
			).Debug("lan server expired")
		}
	}
	if changed {
		s.notifyLocked()
	}
	s.mu.Unlock()
}

// handle records one parsed announcement.
func (s *LanScanner) handle(src netip.AddrPort, payload string) {
	rawMotd, port, ok := parseLanAnnouncement(payload)
	if !ok {
		s.logger.With(
			slog.String("src", src.String()),
			slog.Int("len", len(payload)),
		).Debug("ignoring malformed lan announcement")
		return
	}

	now := time.Now()
	key := lanScanKey{addr: src.Addr(), port: port}

	s.mu.Lock()
	defer s.mu.Unlock()

	self := matchesSelf(s.self, rawMotd, port)
	if srv, ok := s.servers[key]; ok {
		// A repeat. Only wake the UI when something it renders actually moved.
		changed := srv.IsSelf != self || srv.RawMotd != rawMotd ||
			now.Sub(srv.LastSeen) > lanScanStale
		srv.LastSeen = now
		srv.Count++
		srv.RawMotd = rawMotd
		srv.Motd = cleanLanMotd(rawMotd)
		srv.IsSelf = self
		srv.Source = src
		if changed {
			s.notifyLocked()
		}
		return
	}

	s.servers[key] = &LanServer{
		Motd:      cleanLanMotd(rawMotd),
		RawMotd:   rawMotd,
		Port:      port,
		Source:    src,
		Addr:      src.Addr(),
		FirstSeen: now,
		LastSeen:  now,
		Count:     1,
		IsSelf:    self,
	}
	s.logger.With(
		slog.String("addr", src.Addr().String()),
		slog.Int("port", port),
		slog.Bool("self", self),
	).Debug("new lan server")
	s.notifyLocked()
}

// Servers returns the currently-known servers, freshest first, safe to call
// from the UI. The result is a copy: LanServer holds no reference types, so
// the caller may read it without holding any lock.
func (s *LanScanner) Servers() []LanServer {
	s.mu.RLock()
	out := make([]LanServer, 0, len(s.servers))
	for _, srv := range s.servers {
		out = append(out, *srv)
	}
	s.mu.RUnlock()

	// Deterministic ordering keeps the GUI from jittering between refreshes:
	// our own advertisements sink to the bottom, then freshest first.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.IsSelf != b.IsSelf {
			return !a.IsSelf
		}
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.After(b.LastSeen)
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Source.String() < b.Source.String()
	})
	return out
}

// Err returns the last listener error, if the scanner could not bind at all.
// It is empty while the scanner is healthy.
func (s *LanScanner) Err() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// Subscribe returns a channel that receives a value whenever the server set
// meaningfully changes, plus a function that cancels the subscription. The
// channel is buffered and coalescing: a slow reader sees one wakeup, not a
// backlog of duplicate broadcasts.
func (s *LanScanner) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, id)
			s.mu.Unlock()
		})
	}
	return ch, cancel
}

// notifyLocked wakes every subscriber. The caller must hold s.mu.
func (s *LanScanner) notifyLocked() {
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // subscriber has a pending wakeup already
		}
	}
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

// parseLanAnnouncement extracts the MOTD and port from a Minecraft LAN
// broadcast. It is strict: anything not shaped exactly like
// "[MOTD]…[/MOTD][AD]<1..65535>[/AD]" is rejected.
func parseLanAnnouncement(payload string) (motd string, port int, ok bool) {
	motd, ok = between(payload, "[MOTD]", "[/MOTD]")
	if !ok {
		return "", 0, false
	}
	ad, ok := between(payload, "[AD]", "[/AD]")
	if !ok {
		return "", 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(ad))
	if err != nil || !validPort(port) {
		return "", 0, false
	}
	return motd, port, true
}

// between returns the text enclosed by the first open tag and the first close
// tag that follows it.
func between(s, openTag, closeTag string) (string, bool) {
	i := strings.Index(s, openTag)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(openTag):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// cleanLanMotd strips Minecraft section-sign colour codes, trims whitespace and
// caps the result so an oversized announcement cannot distort the UI.
func cleanLanMotd(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	skip := false
	for _, r := range raw {
		if skip {
			// Drop the single formatting character following the section sign.
			skip = false
			continue
		}
		if r == '§' {
			skip = true
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())

	n := 0
	for i := range out {
		n++
		if n > lanScanMotdRunes {
			return out[:i]
		}
	}
	return out
}

// matchesSelf reports whether an announcement corresponds to one of our own
// advertised entries. Comparison uses the raw MOTD, which is exactly what
// core/lan.go puts on the wire.
func matchesSelf(self []LanEntry, rawMotd string, port int) bool {
	for _, e := range self {
		if e.Port == port && e.Motd == rawMotd {
			return true
		}
	}
	return false
}
