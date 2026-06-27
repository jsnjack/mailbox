package ai

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxSSELine bounds a single SSE data line (some providers send large JSON deltas).
const maxSSELine = 1 << 20

// extractFunc pulls the incremental text (if any), a done flag, and any
// provider error event from one SSE data payload. A provider supplies its own
// decoder. A non-nil err means the provider reported a mid-stream error (after a
// 2xx), which must be surfaced rather than silently truncating the result.
type extractFunc func(data []byte) (text string, done bool, err error)

// streamSSE performs req and turns its Server-Sent-Events body into a channel of
// Chunks, using extract to decode each data line. Non-2xx responses return an
// error (with a snippet of the body) before any streaming begins.
func streamSSE(ctx context.Context, client *http.Client, req *http.Request, extract extractFunc) (<-chan Chunk, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("api status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	ch := make(chan Chunk)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()

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
				return
			}
			text, done, eerr := extract([]byte(data))
			if eerr != nil {
				select {
				case ch <- Chunk{Err: eerr}:
				case <-ctx.Done():
				}
				return
			}
			if text != "" {
				select {
				case ch <- Chunk{Text: text}:
				case <-ctx.Done():
					return
				}
			}
			if done {
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			select {
			case ch <- Chunk{Err: fmt.Errorf("read stream: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}
