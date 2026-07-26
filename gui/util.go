package gui

import (
	"runtime"
	"time"
)

// runtimeInfo describes the host, for diagnostic bundle headers.
func runtimeInfo() string {
	return runtime.GOOS + "/" + runtime.GOARCH + " go" + runtime.Version()[2:]
}

// timeSince is time.Since, wrapped so tests can reason about it and so call
// sites in layout code read consistently.
func timeSince(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}
