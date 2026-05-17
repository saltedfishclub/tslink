package core

import (
	"os"
	"strings"
	"time"

	"log/slog"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
)

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func NewLogger(level string, useJsonFormat bool) *slog.Logger {
	w := os.Stdout
	var logger *slog.Logger
	if !useJsonFormat {
		logger = slog.New(tint.NewHandler(colorable.NewColorable(w), &tint.Options{
			Level:      parseLevel(level),
			TimeFormat: time.DateTime,
			//NoColor:    !isatty.IsTerminal(w.Fd()),
		}))
	} else {
		logger = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level: parseLevel(level),
		}))
	}

	slog.SetDefault(logger)
	return logger
}
