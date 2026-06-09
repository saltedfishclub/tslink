package core

import (
	"context"
	"log/slog"

	"tailscale.com/tsnet"
)

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
