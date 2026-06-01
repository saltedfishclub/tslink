package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

const udpForwardIdleTimeout = 2 * time.Minute

func StartForwarders(ctx context.Context, srv *tsnet.Server, rules map[string][]ForwardRule) {
	for tag, rrs := range rules {
		for _, rule := range rrs {
			slog.Info("starting forwarder",
				slog.String("tag", tag),
				slog.String("protocol", rule.Protocol),
				slog.Int("tailscale_port", rule.TailscalePort),
				slog.String("local_addr", rule.LocalAddr),
			)
			go runForwarder(ctx, srv, rule, tag)
		}
	}
}

func StartConnectors(ctx context.Context, srv *tsnet.Server, rules map[string][]ConnectRule) {
	for tag, rrs := range rules {
		for _, rule := range rrs {
			args := []any{
				slog.String("tag", tag),
				slog.String("protocol", rule.Protocol),
				slog.Int("local_port", rule.LocalPort),
				slog.String("dst_addr", rule.DstAddr),
			}
			if rule.LocalAddr != "" {
				args = append(args, slog.String("local_addr", rule.LocalAddr))
			}
			slog.Info("starting connector", args...)
			go runConnector(ctx, srv, rule, tag)
		}
	}
}

func RuleLogger(rule any, tag string) *slog.Logger {
	var args []any
	switch r := rule.(type) {
	case ForwardRule:
		args = []any{
			slog.String("protocol", r.Protocol),
			slog.Int("tailscale_port", r.TailscalePort),
			slog.String("local_addr", r.LocalAddr),
		}
	case ConnectRule:
		args = []any{
			slog.String("protocol", r.Protocol),
			slog.Int("local_port", r.LocalPort),
			slog.String("dst_addr", r.DstAddr),
		}
		if r.LocalAddr != "" {
			args = append(args, slog.String("local_addr", r.LocalAddr))
		}
	default:
		args = []any{
			slog.String("type", fmt.Sprintf("%T", rule)),
		}
	}
	if tag != "" {
		args = append(args, slog.String("tag", tag))
	}
	return slog.With(args...)
}

func runForwarder(ctx context.Context, srv *tsnet.Server, rule ForwardRule, tag string) {
	logger := RuleLogger(rule, tag)

	switch rule.Protocol {
	case "tcp":
		runTCPForwarder(ctx, srv, rule, logger)
	case "udp":
		runUDPForwarder(ctx, srv, rule, logger)
	default:
		logger.Error("unsupported protocol, expected tcp or udp")
	}
}

func runTCPForwarder(ctx context.Context, srv *tsnet.Server, rule ForwardRule, logger *slog.Logger) {
	ip := getSelfTsnetAddr(srv)
	ln, err := srv.Listen("tcp", fmt.Sprintf("%s:%d", ip.String(), rule.TailscalePort))
	if err != nil {
		logger.Error("failed to listen", "error", err)
		return
	}
	logger.Debug("listening", slog.String("on", fmt.Sprintf("tailscale:%s:%d", ip.String(), rule.TailscalePort)))

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("accept error", "error", err)
			continue
		}
		go handleTCPForward(ctx, srv, conn, rule, logger)
	}
}

func handleTCPForward(ctx context.Context, srv *tsnet.Server, conn net.Conn, rule ForwardRule, logger *slog.Logger) {
	remoteAddrStr := conn.RemoteAddr().String()
	clog := logger.With(slog.String("remote", remoteAddrStr))

	lc, err := srv.LocalClient()
	if err == nil {
		who, err := lc.WhoIs(ctx, remoteAddrStr)
		if err == nil {
			clog = clog.With(slog.String("user", who.UserProfile.LoginName))
		}
	}

	connType := getConnType(ctx, srv, remoteAddrStr)
	clog.Info("accepted connection",
		slog.String("conn_type", connType),
		slog.String("local_addr", rule.LocalAddr),
	)

	localConn, err := net.Dial("tcp", rule.LocalAddr)
	if err != nil {
		clog.Error("failed to dial local", "error", err)
		conn.Close()
		return
	}

	stop := context.AfterFunc(ctx, func() {
		conn.Close()
		localConn.Close()
	})
	defer stop()

	toLocal, toTs := pipeConns(conn, localConn)
	clog.Info("connection closed", slog.Int64("ts_rx_bytes", toLocal), slog.Int64("ts_tx_bytes", toTs))
}

var statusCache struct {
	mu      sync.Mutex
	status  *ipnstate.Status
	expires time.Time
}

const statusCacheTTL = 5 * time.Second

func getCachedStatus(ctx context.Context, srv *tsnet.Server) (*ipnstate.Status, error) {
	statusCache.mu.Lock()
	if statusCache.status != nil && time.Now().Before(statusCache.expires) {
		st := statusCache.status
		statusCache.mu.Unlock()
		return st, nil
	}
	statusCache.mu.Unlock()

	lc, err := srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}

	statusCache.mu.Lock()
	statusCache.status = st
	statusCache.expires = time.Now().Add(statusCacheTTL)
	statusCache.mu.Unlock()
	return st, nil
}

func isTsnetTarget(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		tsnetV4 := netip.MustParsePrefix("100.64.0.0/10")
		tsnetV6 := netip.MustParsePrefix("fd7a:115c:a1e0::/48")
		return tsnetV4.Contains(ip) || tsnetV6.Contains(ip)
	}
	return true
}

func getConnType(ctx context.Context, srv *tsnet.Server, remoteAddrStr string) string {
	st, err := getCachedStatus(ctx, srv)
	if err != nil {
		return "unknown"
	}

	remoteHost, _, err := net.SplitHostPort(remoteAddrStr)
	if err != nil {
		return "unknown"
	}

	for _, peer := range st.Peer {
		for _, addr := range peer.TailscaleIPs {
			if addr.String() == remoteHost {
				if peer.CurAddr != "" {
					return "direct"
				}
				if peer.Relay != "" {
					return fmt.Sprintf("derp(%s)", peer.Relay)
				}
				return "direct"
			}
		}
	}
	return "unknown"
}

func runUDPForwarder(ctx context.Context, srv *tsnet.Server, rule ForwardRule, logger *slog.Logger) {
	ip := getSelfTsnetAddr(srv)
	ln, err := srv.Listen("udp", fmt.Sprintf("%s:%d", ip.String(), rule.TailscalePort))
	if err != nil {
		logger.Error("failed to listen", "error", err)
		return
	}
	logger.Debug("listening", slog.String("on", fmt.Sprintf("tailscale:%s:%d", ip.String(), rule.TailscalePort)))

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("accept error", "error", err)
			continue
		}
		go handleUDPForward(ctx, srv, conn, rule, logger)
	}
}

func handleUDPForward(ctx context.Context, srv *tsnet.Server, conn net.Conn, rule ForwardRule, logger *slog.Logger) {
	remoteAddrStr := conn.RemoteAddr().String()
	clog := logger.With(slog.String("remote", remoteAddrStr))

	lc, err := srv.LocalClient()
	if err == nil {
		who, err := lc.WhoIs(ctx, remoteAddrStr)
		if err == nil {
			clog = clog.With(slog.String("user", who.UserProfile.LoginName))
		}
	}

	connType := getConnType(ctx, srv, remoteAddrStr)
	clog.Info("accepted connection",
		slog.String("conn_type", connType),
		slog.String("local_addr", rule.LocalAddr),
	)

	localConn, err := net.Dial("udp", rule.LocalAddr)
	if err != nil {
		clog.Error("failed to dial local", "error", err)
		conn.Close()
		return
	}

	stop := context.AfterFunc(ctx, func() {
		conn.Close()
		localConn.Close()
	})
	defer stop()

	remoteIP, _, _ := net.SplitHostPort(remoteAddrStr)

	var toTs, toLocal int64
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(udpForwardIdleTimeout))
			n, err := conn.Read(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					clog.Debug("udp forward idle timeout on ts side")
				} else if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
					clog.Debug("udp forward read from ts", "error", err)
				}
				localConn.Close()
				return
			}
			_ = conn.SetReadDeadline(time.Time{})
			toLocal += int64(n)
			clog.Debug("inbound udp packet",
				slog.String("from_ip", remoteIP),
				slog.String("to_ip", rule.LocalAddr),
				slog.Int("pkg_size", n),
			)
			if _, err := localConn.Write(buf[:n]); err != nil {
				conn.Close()
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			_ = localConn.SetReadDeadline(time.Now().Add(udpForwardIdleTimeout))
			n, err := localConn.Read(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					clog.Debug("udp forward idle timeout on local side")
				} else if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
					clog.Debug("udp forward read from local", "error", err)
				}
				conn.Close()
				return
			}
			_ = localConn.SetReadDeadline(time.Time{})
			toTs += int64(n)
			localIP, _, _ := net.SplitHostPort(localConn.RemoteAddr().String())
			clog.Debug("outbound udp packet",
				slog.String("from_ip", localIP),
				slog.String("to_ip", remoteIP),
				slog.Int("pkg_size", n),
			)
			if _, err := conn.Write(buf[:n]); err != nil {
				localConn.Close()
				return
			}
		}
	}()

	wg.Wait()
	clog.Info("connection closed", slog.Int64("ts_rx_bytes", toLocal), slog.Int64("ts_tx_bytes", toTs))
}

const udpRelayMaxSessions = 1024

type udpSession struct {
	conn    net.Conn
	remote  net.Addr
	lastUse time.Time
}

type udpRelay struct {
	listenConn net.PacketConn
	dialAddr   string
	logger     *slog.Logger
	direction  string
	srv        *tsnet.Server

	mu       sync.Mutex
	sessions map[string]*udpSession
}

func (r *udpRelay) run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		r.listenConn.Close()
	}()

	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.cleanup()
			}
		}
	}()

	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, from, err := r.listenConn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.logger.Error("udp read error", "error", err)
			return
		}

		key := from.String()
		var toIP string
		r.mu.Lock()
		sess, exists := r.sessions[key]
		if !exists {
			if len(r.sessions) >= udpRelayMaxSessions {
				r.mu.Unlock()
				r.logger.Warn("udp relay session limit reached, dropping packet",
					slog.Int("limit", udpRelayMaxSessions),
					slog.String("remote", key),
				)
				continue
			}
			host, _, err := net.SplitHostPort(r.dialAddr)
			if err != nil {
				r.mu.Unlock()
				r.logger.Error("failed to parse dial addr", "error", err)
				continue
			}
			inTsnet := isTsnetTarget(host)

			r.mu.Unlock()
			var dialed net.Conn
			if inTsnet {
				dialed, err = r.srv.Dial(ctx, "udp", r.dialAddr)
			} else {
				dialed, err = net.Dial("udp", r.dialAddr)
			}
			if err != nil {
				r.logger.Error("failed to dial", "error", err)
				continue
			}
			sess = &udpSession{conn: dialed, remote: from, lastUse: time.Now()}
			r.mu.Lock()
			if existing, dup := r.sessions[key]; dup {
				dialed.Close()
				sess = existing
				sess.lastUse = time.Now()
			} else {
				r.sessions[key] = sess
			}
			toIP = sess.conn.RemoteAddr().String()
			r.mu.Unlock()

			r.logger.Info("new udp session", slog.String("remote", key), slog.String("direction", r.direction))
			go r.readSession(key, sess)
		} else {
			sess.lastUse = time.Now()
			toIP = sess.conn.RemoteAddr().String()
			r.mu.Unlock()
		}

		fromIP, _, _ := net.SplitHostPort(from.String())
		toIPHost, _, _ := net.SplitHostPort(toIP)
		r.logger.Debug("outbound udp packet",
			slog.String("from_ip", fromIP),
			slog.String("to_ip", toIPHost),
			slog.Int("pkg_size", n),
		)
		if _, err := sess.conn.Write(buf[:n]); err != nil {
			r.logger.Error("failed to write", "error", err)
			r.removeSession(key)
		}
	}
}

func (r *udpRelay) readSession(key string, sess *udpSession) {
	buf := make([]byte, 65535)
	for {
		n, err := sess.conn.Read(buf)
		if err != nil {
			r.removeSession(key)
			return
		}
		fromIP, _, _ := net.SplitHostPort(sess.conn.RemoteAddr().String())
		toIP, _, _ := net.SplitHostPort(sess.remote.String())
		r.logger.Info("udp packet",
			slog.String("from_ip", fromIP),
			slog.String("to_ip", toIP),
			slog.Int("pkg_size", n),
		)
		if _, err := r.listenConn.WriteTo(buf[:n], sess.remote); err != nil {
			r.logger.Error("failed to write back", "error", err)
			r.removeSession(key)
			return
		}
		sess.lastUse = time.Now()
	}
}

func (r *udpRelay) removeSession(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[key]; ok {
		remote := s.remote.String()
		s.conn.Close()
		delete(r.sessions, key)
		r.logger.Debug("udp session closed", slog.String("remote", remote))
	}
}

func (r *udpRelay) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	threshold := time.Now().Add(-5 * time.Minute)
	for key, s := range r.sessions {
		if s.lastUse.Before(threshold) {
			remote := s.remote.String()
			s.conn.Close()
			delete(r.sessions, key)
			r.logger.Debug("udp session cleaned up", slog.String("remote", remote))
		}
	}
}

func runConnector(ctx context.Context, srv *tsnet.Server, rule ConnectRule, tag string) {
	logger := RuleLogger(rule, tag)

	switch rule.Protocol {
	case "tcp", "minecraft":
		runTCPConnector(ctx, srv, rule, logger)
	case "udp":
		runUDPConnector(ctx, srv, rule, logger)
	default:
		logger.Error("unsupported protocol, expected tcp or udp")
	}
}

func runTCPConnector(ctx context.Context, srv *tsnet.Server, rule ConnectRule, logger *slog.Logger) {
	bindIP := rule.LocalAddr
	if bindIP == "" {
		bindIP = "0.0.0.0"
	}
	if rule.LANEnabled() && bindIP != "0.0.0.0" {
		logger.Warn("lan_enable forces local_addr to 0.0.0.0, overriding")
		bindIP = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", bindIP, rule.LocalPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("failed to listen locally", "error", err)
		return
	}
	logger.Info("listening", slog.String("on", addr))

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("accept error", "error", err)
			continue
		}
		go handleTCPConnect(ctx, srv, conn, rule, logger)
	}
}

func handleTCPConnect(ctx context.Context, srv *tsnet.Server, conn net.Conn, rule ConnectRule, logger *slog.Logger) {
	clog := logger.With(slog.String("local_client", conn.RemoteAddr().String()))

	tsConn, err := srv.Dial(ctx, "tcp", rule.DstAddr)
	if err != nil {
		clog.Error("failed to dial tailscale", "error", err)
		conn.Close()
		return
	}

	stop := context.AfterFunc(ctx, func() {
		conn.Close()
		tsConn.Close()
	})
	defer stop()

	clog.Info("accepted connection", slog.String("dst_addr", rule.DstAddr))
	toConn, toTs := pipeConns(conn, tsConn)
	clog.Info("connection closed", slog.Int64("ts_rx_bytes", toTs), slog.Int64("ts_tx_bytes", toConn))
}

func runUDPConnector(ctx context.Context, srv *tsnet.Server, rule ConnectRule, logger *slog.Logger) {
	bindIP := rule.LocalAddr
	if bindIP == "" {
		bindIP = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", bindIP, rule.LocalPort)
	addrUDP, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		logger.Error("failed to resolve local addr", "error", err)
		return
	}

	pc, err := net.ListenUDP("udp", addrUDP)
	if err != nil {
		logger.Error("failed to listen locally", "error", err)
		return
	}
	logger.Info("listening", slog.String("on", addr))

	relay := &udpRelay{
		listenConn: pc,
		dialAddr:   rule.DstAddr,
		logger:     logger,
		direction:  "tailscale",
		srv:        srv,
		sessions:   make(map[string]*udpSession),
	}
	relay.run(ctx)
}

func pipeConns(a, b net.Conn) (toA, toB int64) {
	done := make(chan struct{}, 2)
	var aToB, bToA int64

	go func() {
		defer func() { done <- struct{}{} }()
		n, err := io.Copy(a, b)
		aToB = n
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			slog.Debug("pipe copy error", "direction", "b->a", "error", err)
		}
		if tc, ok := a.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			a.Close()
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		n, err := io.Copy(b, a)
		bToA = n
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			slog.Debug("pipe copy error", "direction", "a->b", "error", err)
		}
		if tc, ok := b.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			b.Close()
		}
	}()

	<-done
	<-done
	return aToB, bToA
}
