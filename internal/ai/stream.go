package ai

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// maxSSELine bounds a single SSE data line (some providers send large JSON deltas).
const maxSSELine = 1 << 20

// extractFunc pulls the incremental text (if any), a done flag, and any
// provider error event from one SSE data payload. A provider supplies its own
// decoder. A non-nil err means the provider reported a mid-stream error (after a
// 2xx), which must be surfaced rather than silently truncating the result.
type extractFunc func(data []byte) (text string, done bool, err error)

// streamSSE performs req and turns its Server-Sent-Events body into a channel of
// Chunks, using extract to decode each data line. Non-2xx responses return a
// status-only error before any streaming begins (the body goes to the trace).
func streamSSE(ctx context.Context, client *http.Client, req *http.Request, extract extractFunc, provider, model string) (<-chan Chunk, error) {
	reqStart := time.Now()
	logging.Trace("ai: request", "provider", provider, "model", model,
		"url", req.URL.String(), "bytes", req.ContentLength)
	resp, err := client.Do(req)
	if err != nil {
		logging.Trace("ai: request failed", "provider", provider, "model", model,
			"url", req.URL.String(), "dur", time.Since(reqStart), "err", err)
		return nil, fmt.Errorf("request: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// The error carries only the status: the body can be a whole CDN error
		// page (2KB of Cloudflare HTML, measured), which the activity log would
		// render line by line. The trace keeps the full body for debugging.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		logging.Trace("ai: response error", "provider", provider, "model", model,
			"status", resp.StatusCode, "dur", time.Since(reqStart),
			"body", logging.Body(strings.TrimSpace(string(b))))
		return nil, fmt.Errorf("api status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	logging.Trace("ai: response", "provider", provider, "model", model,
		"status", resp.StatusCode, "dur", time.Since(reqStart))

	ch := make(chan Chunk)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()

		streamStart := time.Now()
		logging.Trace("ai: stream start", "provider", provider, "model", model)
		var chunks, bytesOut int
		var think thinkFilter // drops inline chain-of-thought, see think.go
		// Registered after close(ch) so it runs BEFORE it (defers are LIFO):
		// releases text the filter withheld while it looked like the start of a
		// <think> tag but turned out to be ordinary content at end of stream.
		defer func() {
			if tail := think.flush(); tail != "" {
				select {
				case ch <- Chunk{Text: tail}:
				case <-ctx.Done():
				}
			}
		}()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				logging.Trace("ai: stream end", "provider", provider, "model", model,
					"chunks", chunks, "bytes", bytesOut, "dur", time.Since(streamStart), "status", "done")
				return
			}
			text, done, eerr := extract([]byte(data))
			if eerr != nil {
				logging.Trace("ai: stream error", "provider", provider, "model", model,
					"chunks", chunks, "bytes", bytesOut, "dur", time.Since(streamStart), "err", eerr)
				select {
				case ch <- Chunk{Err: eerr}:
				case <-ctx.Done():
				}
				return
			}
			if text != "" {
				text = think.feed(text)
			}
			if text != "" {
				chunks++
				bytesOut += len(text)
				select {
				case ch <- Chunk{Text: text}:
				case <-ctx.Done():
					logging.Trace("ai: stream cancelled", "provider", provider, "model", model,
						"chunks", chunks, "bytes", bytesOut, "dur", time.Since(streamStart), "err", ctx.Err())
					return
				}
			}
			if done {
				logging.Trace("ai: stream end", "provider", provider, "model", model,
					"chunks", chunks, "bytes", bytesOut, "dur", time.Since(streamStart), "status", "done")
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			logging.Trace("ai: stream read error", "provider", provider, "model", model,
				"chunks", chunks, "bytes", bytesOut, "dur", time.Since(streamStart), "err", err)
			select {
			case ch <- Chunk{Err: fmt.Errorf("read stream: %w", err)}:
			case <-ctx.Done():
			}
			return
		}
		logging.Trace("ai: stream end", "provider", provider, "model", model,
			"chunks", chunks, "bytes", bytesOut, "dur", time.Since(streamStart), "status", "eof")
	}()
	return ch, nil
}
