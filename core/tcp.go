package core

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"tailscale.com/tsnet"
)

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

	localConn, err := dialTCP(ctx, rule.LocalAddr)
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

func runTCPConnector(ctx context.Context, srv *tsnet.Server, rule ConnectRule, logger *slog.Logger) {
	bindIP := rule.BindIP()
	if rule.LANEnabled() && rule.LocalAddr != "" && rule.LocalAddr != "0.0.0.0" {
		logger.Warn("lan_enable forces local_addr to 0.0.0.0, overriding")
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

	tsConn, err := dialTsnet(ctx, srv, "tcp", rule.DstAddr)
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
