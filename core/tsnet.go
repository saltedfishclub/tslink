package core

import (
	"context"
	fmt2 "fmt"
	"log/slog"

	"tailscale.com/ipn"

	"tailscale.com/tsnet"
)

func InitTsNet(ctx context.Context, cfg *Core, logger *slog.Logger) (*tsnet.Server, error) {
	srv := &tsnet.Server{
		Hostname:  "tslink-" + cfg.Hostname,
		AuthKey:   cfg.AuthKey,
		Ephemeral: cfg.Ephemeral,
		Logf: func(fmt string, args ...interface{}) {
			logger.With(slog.String("from", "tsnet")).Debug(fmt2.Sprintf(fmt, args...))
		},
		UserLogf: func(fmt string, args ...interface{}) {
			logger.With(slog.String("from", "tsnet")).Info(fmt2.Sprintf(fmt, args...))
		},
		RunWebClient: true,
	}

	if cfg.ControlURL != "" {
		srv.ControlURL = cfg.ControlURL
	}

	logger.Debug("starting tsnet server")
	if err := srv.Start(); err != nil {
		logger.With(slog.String("error", err.Error())).Error("starting tsnet server failed")
		return nil, err
	}

	status, err := srv.Up(ctx)
	if err != nil {
		logger.With(slog.String("error", err.Error())).Error("bring up tsnet server failed")
		return nil, err
	}

	for _, ip := range status.TailscaleIPs {
		logger.With(slog.String("ip", ip.String())).Info("ip got from tsnet")
	}

	if cfg.AcceptRoutes {
		lc, err := srv.LocalClient()
		if err != nil {
			logger.With(slog.String("error", err.Error())).Error("error from getting local client")
		} else {
			_, err = lc.EditPrefs(ctx, &ipn.MaskedPrefs{
				Prefs:       ipn.Prefs{RouteAll: true},
				RouteAllSet: true,
			})
			if err != nil {
				logger.With(slog.String("error", err.Error())).Error("error from editing prefs")
			} else {
				logger.Debug("subnet route accepted")
			}
		}
	}

	return srv, nil
}
