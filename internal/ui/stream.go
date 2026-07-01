package ui

import (
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
)

// streamFlushInterval bounds how often a streaming AI response repaints its
// widget. Tokens arrive far faster than this (tens per second); without
// coalescing each one triggers a dispatch.Main hop plus a full-widget rewrite,
// which makes the stream visibly choppy. ~33ms ≈ 30fps: smooth, but a fraction
// of the main-thread work.
const streamFlushInterval = 33 * time.Millisecond

// streamCoalesced consumes an AI chunk channel on the calling (background)
// goroutine, accumulating the text and invoking flush with the full text so far
// on the GTK main thread at most once per streamFlushInterval. It returns the
// final accumulated text and the first stream error (nil on success); the caller
// does the final render (and any caching/guards) after it returns.
//
// flush runs on the main thread (it is dispatched via dispatch.Main); it must
// re-check any staleness guards itself, since the stream it belongs to may have
// been superseded by the time the idle callback runs.
func streamCoalesced(ch <-chan ai.Chunk, flush func(text string)) (string, error) {
	logging.Trace("ui: ai stream begin")
	var acc strings.Builder
	var firstErr error
	var last time.Time
	var chunks int
	for c := range ch {
		if c.Err != nil {
			firstErr = c.Err
			break
		}
		chunks++
		acc.WriteString(c.Text)
		if time.Since(last) >= streamFlushInterval {
			last = time.Now()
			snap := acc.String() // immutable snapshot; acc keeps growing here
			dispatch.Main(func() { flush(snap) })
		}
	}
	logging.Trace("ui: ai stream end", "chunks", chunks, "bytes", acc.Len(), "err", firstErr)
	return acc.String(), firstErr
}
