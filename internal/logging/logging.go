// Package logging centralizes structured-logging setup so every layer shares
// one trace level and one handler. The entry point wires Init from the
// --debug/--trace flags; internal packages log via slog.Default().
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// LevelTrace is the most verbose level, below slog.LevelDebug. Trace output is
// written only to the trace file so wire-level detail never reaches the terminal.
const LevelTrace = slog.Level(-8)

// bodyCap bounds how many bytes of a large value (an email body, a MIME blob, a
// raw API payload) a single trace line may contain, so one log entry can't dump
// megabytes even though trace deliberately records full content.
const bodyCap = 2048

// Trace logs at LevelTrace via the default logger. slog exposes no Trace method,
// so this wraps Log(ctx, LevelTrace, …) to give every package one ergonomic,
// consistent call for the finest-grained "what is happening right now" detail.
// Keep the message "pkg: action" and pass structured key/value args (ids,
// counts, sizes, durations, errors) — see AGENTS.md "Tracing".
func Trace(msg string, args ...any) {
	slog.Default().Log(context.Background(), LevelTrace, msg, args...)
}

// TraceContext is Trace with an explicit context (carries cancellation/deadline
// into the handler and any future context-scoped log attributes).
func TraceContext(ctx context.Context, msg string, args ...any) {
	slog.Default().Log(ctx, LevelTrace, msg, args...)
}

// Enabled reports whether trace-level logging is currently active. Guard with it
// only around genuinely expensive arg construction (hashing, re-serialising) —
// slog already skips disabled levels, and eager arg evaluation for cheap values
// (ids, counts, Body of an already-in-hand string) is not worth guarding.
func Enabled() bool {
	return slog.Default().Enabled(context.Background(), LevelTrace)
}

// Body caps a possibly-large string (email body, MIME part, raw payload) to
// bodyCap bytes for a trace line, appending a "…(+N more bytes)" marker when it
// truncates. Use it for any value whose length is caller-controlled or unbounded.
func Body(s string) string {
	if len(s) <= bodyCap {
		return s
	}
	extra := len(s) - bodyCap
	return s[:bodyCap] + "…(+" + itoa(extra) + " more bytes)"
}

// itoa avoids pulling strconv into this leaf package for one small use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

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
	// Render our custom LevelTrace as "TRACE" instead of slog's default
	// "DEBUG-4", so the trace file reads cleanly for a human or an agent.
	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				if lv, ok := a.Value.Any().(slog.Level); ok && lv == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	}
	h := slog.NewTextHandler(w, opts)
	slog.SetDefault(slog.New(h))
	return cleanup
}
