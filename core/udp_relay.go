package core

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

const udpRelayMaxSessions = 1024

type udpSession struct {
	conn    net.Conn
	remote  net.Addr
	mu      sync.Mutex
	lastUse time.Time
}

func (s *udpSession) touch() {
	s.mu.Lock()
	s.lastUse = time.Now()
	s.mu.Unlock()
}

func (s *udpSession) idleSince(threshold time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUse.Before(threshold)
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
				dialed, err = dialTsnet(ctx, r.srv, "udp", r.dialAddr)
			} else {
				dialed, err = dialUDP(ctx, r.dialAddr)
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
				sess.touch()
			} else {
				r.sessions[key] = sess
			}
			toIP = sess.conn.RemoteAddr().String()
			r.mu.Unlock()

			r.logger.Info("new udp session", slog.String("remote", key), slog.String("direction", r.direction))
			go r.readSession(key, sess)
		} else {
			sess.touch()
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
		sess.touch()
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
		if s.idleSince(threshold) {
			remote := s.remote.String()
			s.conn.Close()
			delete(r.sessions, key)
			r.logger.Debug("udp session cleaned up", slog.String("remote", remote))
		}
	}
}
