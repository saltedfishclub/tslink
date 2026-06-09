package core

import (
	"context"
	"fmt"
	"log/slog"

	"tailscale.com/tsnet"
)

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
