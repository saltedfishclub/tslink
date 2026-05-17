package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	"tslink/core"
)

func serviceLogic(configPath string, logger *slog.Logger) bool {
	cfg, err := core.LoadConfig(configPath)
	if err != nil {
		logger.With(
			slog.String("error", err.Error()),
		).Error("Error loading config")
		os.Exit(1)
	}

	ctx, cancelAll := context.WithCancel(context.Background())
	logger.Info("initializing tsnet server")
	srv, err := core.InitTsNet(ctx, &cfg.Core, logger)
	if err != nil {
		logger.With(
			slog.String("error", err.Error())).Error("Error initializing tsnet")
		os.Exit(1)
	}
	logger.Info("tsnet server initialized")

	core.StartForwarders(ctx, srv, cfg.Forward)
	core.StartConnectors(ctx, srv, cfg.Connect)

	core.RunLanDiscoverService(ctx, cfg.Connect, logger.With("from", "lan_service"))

	sigHandler := make(chan os.Signal, 1)
	signal.Notify(sigHandler, os.Interrupt, syscall.SIGTERM)

	watchDog := core.StartTimeWatchDog(ctx, logger.With("from", "watchdog"))

	go func() {
		<-ctx.Done()
		logger.Debug("stopping tsnet service")
		srv.Close()
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
	logLevel := flag.String("level", "info", "log level (DEBUG|INFO|WARN|ERROR)")
	configPath := flag.String("c", "config.toml", "path to config file")
	flag.Parse()

	logger := core.NewLogger(*logLevel, *useJsonFormatLogger)

	logger.Info("Starting tslink server", "level", *logLevel, "configPath", *configPath)

	for {
		isStopped := serviceLogic(*configPath, logger)
		if !isStopped {
			logger.Warn("tslink server restart")
		} else {
			return
		}
	}
}
