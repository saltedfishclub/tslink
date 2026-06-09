package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

const udpForwardIdleTimeout = 2 * time.Minute

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

	localConn, err := dialUDP(ctx, rule.LocalAddr)
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

func runUDPConnector(ctx context.Context, srv *tsnet.Server, rule ConnectRule, logger *slog.Logger) {
	bindIP := rule.BindIP()
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
