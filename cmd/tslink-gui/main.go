// Command tslink-gui is the desktop front-end for tslink.
//
// It runs the same service the headless binary does — see the root main.go —
// but supervises it in-process so the window can show tailnet peer health,
// latency history, Minecraft servers announced on the LAN, and a full network
// diagnostic run, plus a searchable log view that can be shared to a paste
// service for support.
//
// The UI is Gio: no webview, no bundled browser, one native binary per
// platform.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gioui.org/app"

	"tslink/core"
	"tslink/gui"
)

// startPprof exposes net/http/pprof for diagnosing the GUI itself — frame-rate
// regressions in a GPU-accelerated UI are very hard to reason about without a
// profile.
//
// It refuses to bind anywhere but loopback: these handlers expose goroutine
// stacks and allow anyone who can reach them to trigger expensive profiles.
func startPprof(addr string, logger *slog.Logger) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		logger.Error("invalid -pprof address, expected host:port", "addr", addr, "err", err)
		return
	}
	if !isLoopbackHost(host) {
		logger.Error("refusing to serve pprof on a non-loopback address", "addr", addr)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Warn("pprof endpoint enabled", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("pprof server stopped", "err", err)
		}
	}()
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Version is stamped at build time:
//
//	go build -ldflags "-X main.Version=v1.2.3" ./cmd/tslink-gui
var Version = "dev"

func main() {
	var (
		configPath  = flag.String("c", "config.toml", "path to config file")
		configURL   = flag.String("config-url", core.DefaultConfigURL, "URL to fetch config from")
		logLevel    = flag.String("level", "info", "console log level (DEBUG|INFO|WARN|ERROR)")
		jsonFormat  = flag.Bool("json-format", false, "use json format for the console logger")
		tsnetDebug  = flag.Bool("diagnose", false, "show tsnet debug log on level=debug")
		ipinfoToken = flag.String("ipinfo-token", os.Getenv("IPINFO_TOKEN"),
			"optional ipinfo.io token, raises the geolocation rate limit")
		light  = flag.Bool("light", false, "start in the light theme")
		pprofA = flag.String("pprof", "", "serve net/http/pprof on this address, e.g. 127.0.0.1:6060 (loopback only)")
	)
	flag.Parse()

	// The ring buffer captures at debug level regardless of what the console
	// prints, so the log view and any shared bundle have the detail even when
	// the user started without -level=debug.
	logs := core.NewLogBuffer(core.DefaultLogCapacity)
	logger := core.NewLoggerWithBuffer(*logLevel, *jsonFormat, logs)
	logger.Info("starting tslink gui",
		"version", Version,
		"level", *logLevel,
		"config", *configPath,
	)

	if *pprofA != "" {
		startPprof(*pprofA, logger)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sup := core.NewSupervisor(core.SupervisorOptions{
		ConfigPath: *configPath,
		ConfigURL:  *configURL,
		TsnetDebug: *tsnetDebug,
		Logger:     logger,
	})
	go sup.Run(ctx)

	ui := gui.New(gui.Options{
		Version:     Version,
		ConfigPath:  *configPath,
		ConfigURL:   *configURL,
		Supervisor:  sup,
		Logs:        logs,
		Logger:      logger,
		IPInfoToken: *ipinfoToken,
		StartDark:   !*light,
	})

	go func() {
		err := ui.Run(ctx)
		// Closing the window shuts the service down: the GUI is the process.
		cancel()
		if err != nil {
			logger.Error("gui exited", "err", err)
			log.SetFlags(0)
			os.Exit(1)
		}
		os.Exit(0)
	}()

	app.Main()
}
