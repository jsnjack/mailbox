// Package logging centralizes structured-logging setup so every layer shares
// one trace level and one handler. The entry point wires Init from the
// --debug/--trace flags; internal packages log via slog.Default().
package logging

import (
	"io"
	"log/slog"
	"os"
)

// LevelTrace is the most verbose level, below slog.LevelDebug. Trace output is
// written only to the trace file so wire-level detail never reaches the terminal.
const LevelTrace = slog.Level(-8)

// Init configures the global slog logger from the resolved trace path and level
// and returns a cleanup function that must be deferred. When tracePath is set,
// logs go there (truncated each run); otherwise debug/warn output goes to
// stderr. An empty level means warnings and errors only.
func Init(tracePath, level string) func() {
	var w io.Writer = os.Stderr
	cleanup := func() {}
	if tracePath != "" {
		f, err := os.OpenFile(tracePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err == nil {
			w = f
			cleanup = func() { _ = f.Close() }
		}
	}
	lvl := slog.LevelWarn
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "trace":
		lvl = LevelTrace
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
	return cleanup
}
