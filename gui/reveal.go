package gui

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// revealTimeout bounds the helper process. A missing or wedged file manager
// must not leave a goroutine parked forever.
const revealTimeout = 10 * time.Second

// RevealInFileManager opens the platform file manager with path selected,
// falling back to opening its containing directory.
//
// Writing a log file and only printing where it went is not much use to someone
// who is about to attach it to a bug report, so the export shows it instead of
// describing it.
//
// It blocks; callers should run it off the UI goroutine.
func RevealInFileManager(path string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(context.Background(), revealTimeout)
	defer cancel()

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	switch runtime.GOOS {
	case "darwin":
		// -R reveals rather than opens, so Finder highlights the file.
		return exec.CommandContext(ctx, "open", "-R", abs).Run()

	case "windows":
		// explorer wants the comma glued to the flag, and exits non-zero even
		// when it succeeds, so its status is deliberately ignored.
		_ = exec.CommandContext(ctx, "explorer", "/select,"+abs).Run()
		return nil

	default:
		// The freedesktop interface highlights the file; every major Linux file
		// manager implements it. Fall back to opening the directory when the
		// service is absent — dbus-send itself may not even be installed.
		uri := "file://" + abs
		dbus := exec.CommandContext(ctx, "dbus-send",
			"--session", "--dest=org.freedesktop.FileManager1", "--type=method_call",
			"/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems",
			"array:string:"+uri, "string:tslink",
		)
		if err := dbus.Run(); err == nil {
			return nil
		} else {
			logger.Debug("FileManager1.ShowItems unavailable, opening the directory",
				"err", err)
		}
		return exec.CommandContext(ctx, "xdg-open", filepath.Dir(abs)).Run()
	}
}
