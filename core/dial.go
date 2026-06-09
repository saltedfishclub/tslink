package core

import (
	"context"
	"net"
	"time"

	"tailscale.com/tsnet"
)

const dialTimeout = 10 * time.Second

func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	return dialer.DialContext(ctx, "tcp", addr)
}

func dialUDP(ctx context.Context, addr string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	return dialer.DialContext(ctx, "udp", addr)
}

func dialTsnet(ctx context.Context, srv *tsnet.Server, network, addr string) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return srv.Dial(dialCtx, network, addr)
}
