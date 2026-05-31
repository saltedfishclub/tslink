package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	"tslink/core"
)

func serviceLogic(configPath string, isTsnetDebug bool, configURL string, logger *slog.Logger) bool {
	// Determine config source: URL takes priority, then file
	configSource := configPath
	if configURL != "" {
		configSource = configURL
		logger.Info("Using config URL", "url", configURL)
	}

	cfg, err := core.LoadConfig(configSource)
	if err != nil {
		logger.With(
			slog.String("error", err.Error()),
		).Error("Error loading config")
		os.Exit(1)
	}

	ctx, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()
	logger.Info("initializing tsnet server")
	srv, err := core.InitTsNet(ctx, &cfg.Core, logger, isTsnetDebug)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Debug("tsnet initialization timed out, retrying")
			return false
		}
		logger.With(
			slog.String("error", err.Error())).Error("Error initializing tsnet")
		os.Exit(1)
	}
	logger.Info("tsnet server initialized")

	core.StartForwarders(ctx, srv, cfg.Forward)
	core.StartConnectors(ctx, srv, cfg.Connect)

	core.RunLanDiscoverService(ctx, cfg.Connect, logger.With("from", "lan_service"))

	core.StartPeerConnectivityDiagnostics(ctx, logger, srv, cfg.Connect)

	sigHandler := make(chan os.Signal, 1)
	signal.Notify(sigHandler, os.Interrupt, syscall.SIGTERM)

	watchDog := core.StartTimeWatchDog(ctx, logger.With("from", "watchdog"))

	go func() {
		<-ctx.Done()
		logger.Debug("stopping tsnet service")
		_ = srv.Close() // just ignore it
	}()

	for {
		select {
		case <-sigHandler:
			logger.Warn("Ctrl+C received, shutting down")
			cancelAll()
			time.Sleep(time.Second)
			return true
		case <-watchDog:
			logger.Warn("WatchDog trigged, restarting service")
			cancelAll()
			time.Sleep(time.Second)
			return false
		}
	}
}

func main() {
	useJsonFormatLogger := flag.Bool("json-format", false, "use json format logger")
	showTsnetDebugLog := flag.Bool("diagnose", false, "show tsnet debug log on level=debug")
	logLevel := flag.String("level", "info", "log level (DEBUG|INFO|WARN|ERROR)")
	configPath := flag.String("c", "config.toml", "path to config file")
	configURL := flag.String("config-url", core.DefaultConfigURL, "URL to fetch config from (default from build ldflags)")
	flag.Parse()

	logger := core.NewLogger(*logLevel, *useJsonFormatLogger)

	logger.Info("Starting tslink server", "level", *logLevel, "configPath", *configPath)

	for {
		isStopped := serviceLogic(*configPath, *showTsnetDebugLog, *configURL, logger)
		if !isStopped {
			logger.Warn("tslink server restart")
		} else {
			return
		}
	}
}
