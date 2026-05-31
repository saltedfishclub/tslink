package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	"tslink/core"

	"gioui.org/app"
)

func serviceLogic(configPath string, logger *slog.Logger) bool {
	core.Events <- &core.LinkInitEvent{
		State: core.LinkInitFetchConfig,
	}
	cfg, err := core.LoadConfig(configPath)
	if err != nil {
		logger.With(
			slog.String("error", err.Error()),
		).Error("Error loading config")
		core.Events <- &core.LinkErrorEvent{Error: fmt.Sprintf("Load config: %v", err)}
		return true
	}

	ctx, cancelAll := context.WithCancel(context.Background())
	logger.Info("initializing tsnet server")
	srv, err := core.InitTsNet(ctx, &cfg.Core, logger)
	if err != nil {
		logger.With(
			slog.String("error", err.Error())).Error("Error initializing tsnet")
		core.Events <- &core.LinkErrorEvent{Error: fmt.Sprintf("Init tsnet: %v", err)}
		return true
	}
	logger.Info("tsnet server initialized")

	core.Events <- &core.LinkInitEvent{State: core.LinkInitProgramSetup}
	core.StartForwarders(ctx, srv, cfg.Forward)
	core.StartConnectors(ctx, srv, cfg.Connect)

	core.RunLanDiscoverService(ctx, cfg.Connect, logger.With("from", "lan_service"))

	core.StartPeerConnectivityDiagnostics(ctx, logger, srv, cfg.Connect)

	core.Events <- &core.LinkInitEvent{State: core.LinkInitReady}
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
	core.Events = make(chan interface{}, 4)
	useJsonFormatLogger := flag.Bool("json-format", false, "use json format logger")
	logLevel := flag.String("level", "info", "log level (DEBUG|INFO|WARN|ERROR)")
	configPath := flag.String("c", "config.toml", "path to config file")
	headless := flag.Bool("headless", false, "use headless mode (no gui)")
	flag.Parse()

	logger := core.NewLogger(*logLevel, *useJsonFormatLogger)
	logger.Info("Starting tslink server", "level", *logLevel, "configPath", *configPath)

	if *headless {
		go func() {
			select {
			case <-core.Events:
				// ignore.
			}
		}()
	} else {
		go func() {
			window := new(app.Window)
			window.Option(app.Title("tslink"))
			if err := run(window); err != nil {
				logger.Error("window error", "error", err)
				os.Exit(1)
			}
			os.Exit(0)
		}()
		go func() {
			for {
				isStopped := serviceLogic(*configPath, logger)
				if isStopped {
					os.Exit(0)
				}
				logger.Warn("tslink server restart")
			}
		}()
		app.Main()
	}
}
