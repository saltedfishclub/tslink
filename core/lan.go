package core

import (
	"context"
	"fmt"
	"net"
	"time"

	"log/slog"
)

type LanEntry struct {
	Motd string
	Port int
}

func LanDiscoverService(ctx context.Context, entryList []LanEntry, logger *slog.Logger) {
	mcastAddrs := []string{
		"224.0.2.60:4445",
		"[ff75:230::60]:4445",
	}

	var fdList []*net.UDPConn
	for _, addrStr := range mcastAddrs {
		addr, err := net.ResolveUDPAddr("udp", addrStr)
		if err != nil {
			logger.With(slog.String("error", err.Error())).Error("failed to resolve udp address")
			continue
		}
		fd, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			logger.With(slog.String("error", err.Error())).Error("failed to dial udp server")
			continue
		}
		fdList = append(fdList, fd)
	}

	if len(fdList) == 0 {
		// init fail
		logger.Warn("all multicast binding failed, service discovery is disabled")
		return
	}

	for _, entry := range entryList {
		// debug log to print entry detail
		logger.With(
			slog.Int("port", entry.Port),
			slog.String("motd", entry.Motd),
		).Debug("discover service: %s on %d", entry.Motd, entry.Port)
	}

	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Debug("shutting down lan discovery service")
			for _, fd := range fdList {
				fd.Close()
			}
			return
		case <-ticker.C:
			for _, e := range entryList {
				msg := fmt.Sprintf("[MOTD]%s[/MOTD][AD]%d[/AD]", e.Motd, e.Port)
				for _, c := range fdList {
					_, err := c.Write([]byte(msg))
					if err != nil {
						logger.With(slog.String("error", err.Error())).Error("failed to write to udp server")
						return
					}
				}
			}
		}
	}
}

func RunLanDiscoverService(ctx context.Context, rules map[string][]ConnectRule, logger *slog.Logger) {
	var lanEntries []LanEntry
	for tag, rs := range rules {
		for _, rule := range rs {
			if !rule.LANEnabled() {
				continue
			}
			motd := rule.LANMotdOr(tag)
			lanEntries = append(lanEntries, LanEntry{
				Motd: motd,
				Port: rule.LocalPort,
			})
		}
	}

	go LanDiscoverService(ctx, lanEntries, logger)
}
