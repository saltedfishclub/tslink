package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

func StartForwarders(ctx context.Context, srv *tsnet.Server, rules map[string][]ForwardRule) {
	for tag, rrs := range rules {
		for _, rule := range rrs {
			rule := rule
			tag := tag
			slog.Debug("starting forwarder",
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
			rule := rule
			tag := tag
			args := []any{
				slog.String("tag", tag),
				slog.String("protocol", rule.Protocol),
				slog.Int("local_port", rule.LocalPort),
				slog.String("dst_addr", rule.DstAddr),
			}
			if rule.LocalAddr != "" {
				args = append(args, slog.String("local_addr", rule.LocalAddr))
			}
			slog.Debug("starting connector", args...)
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
	ip4, ip6 := srv.TailscaleIPs()
	ip := ip4
	if !ip.IsValid() {
		ip = ip6
	}
	ln, err := srv.Listen("tcp", fmt.Sprintf("%s:%d", ip.String(), rule.TailscalePort))
	if err != nil {
		logger.Error("failed to listen", "error", err)
		return
	}
	logger.Info("listening", slog.String("on", fmt.Sprintf("tailscale:%s:%d", ip.String(), rule.TailscalePort)))

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

	defer conn.Close()

	localConn, err := net.Dial("tcp", rule.LocalAddr)
	if err != nil {
		clog.Error("failed to dial local", "error", err)
		return
	}
	defer localConn.Close()

	toTs, toLocal := pipeConns(conn, localConn)
	clog.Info("connection closed", slog.Int64("ts_rx_bytes", toLocal), slog.Int64("ts_tx_bytes", toTs))
}

func getConnType(ctx context.Context, srv *tsnet.Server, remoteAddrStr string) string {
	lc, err := srv.LocalClient()
	if err != nil {
		return "unknown"
	}
	status, err := lc.Status(ctx)
	if err != nil {
		return "unknown"
	}

	remoteHost, _, err := net.SplitHostPort(remoteAddrStr)
	if err != nil {
		return "unknown"
	}

	for _, peer := range status.Peer {
		for _, addr := range peer.TailscaleIPs {
			if addr.String() == remoteHost {
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
	ip4, ip6 := srv.TailscaleIPs()
	ip := ip4
	if !ip.IsValid() {
		ip = ip6
	}
	pc, err := srv.ListenPacket("udp", fmt.Sprintf("%s:%d", ip.String(), rule.TailscalePort))
	if err != nil {
		logger.Error("failed to listen", "error", err)
		return
	}
	logger.Info("listening", slog.String("on", fmt.Sprintf("tailscale:%s:%d", ip.String(), rule.TailscalePort)))

	relay := &udpRelay{
		listenConn: pc,
		dialAddr:   rule.LocalAddr,
		logger:     logger,
		direction:  "local",
		sessions:   make(map[string]*udpSession),
	}
	relay.run(ctx)
}

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
		r.mu.Lock()
		sess, exists := r.sessions[key]
		if !exists {
			dialed, err := net.Dial("udp", r.dialAddr)
			if err != nil {
				r.mu.Unlock()
				r.logger.Error("failed to dial", "error", err)
				continue
			}
			sess = &udpSession{conn: dialed, remote: from, lastUse: time.Now()}
			r.sessions[key] = sess
			r.mu.Unlock()

			r.logger.Debug("new udp session", slog.String("remote", key), slog.String("direction", r.direction))
			go r.readSession(key, sess)
		} else {
			sess.lastUse = time.Now()
			r.mu.Unlock()
		}

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
		if _, err := r.listenConn.WriteTo(buf[:n], sess.remote); err != nil {
			r.logger.Error("failed to write back", "error", err)
			r.removeSession(key)
			return
		}
	}
}

func (r *udpRelay) removeSession(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[key]; ok {
		s.conn.Close()
		delete(r.sessions, key)
		r.logger.Debug("udp session closed", slog.String("remote", key))
	}
}

func (r *udpRelay) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	threshold := time.Now().Add(-5 * time.Minute)
	for key, s := range r.sessions {
		if s.lastUse.Before(threshold) {
			s.conn.Close()
			delete(r.sessions, key)
			r.logger.Debug("udp session cleaned up", slog.String("remote", key))
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
	defer conn.Close()

	tsConn, err := srv.Dial(ctx, "tcp", rule.DstAddr)
	if err != nil {
		clog.Error("failed to dial tailscale", "error", err)
		return
	}
	defer tsConn.Close()

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
		sessions:   make(map[string]*udpSession),
	}
	relay.run(ctx)
}

func pipeConns(a, b net.Conn) (toA, toB int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(a, b)
		toA = n
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(b, a)
		toB = n
	}()
	wg.Wait()
	return
}
