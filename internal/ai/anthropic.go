package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

const (
	anthropicVersion   = "2023-06-01"
	anthropicMaxTokens = 4096
)

// anthropicProvider speaks the Anthropic Messages API.
type anthropicProvider struct {
	client   *http.Client
	xfer     *transferCounter
	endpoint string // base URL including /v1
	apiKey   string
	model    string
}

func newAnthropicProvider(endpoint, apiKey, model string) *anthropicProvider {
	xfer := &transferCounter{}
	return &anthropicProvider{
		client:   countingClient(120*time.Second, xfer),
		xfer:     xfer,
		endpoint: endpoint,
		apiKey:   apiKey,
		model:    model,
	}
}

func (p *anthropicProvider) transfer() (in, out int64) { return p.xfer.snapshot() }

func (p *anthropicProvider) Name() string { return "anthropic" }

func (p *anthropicProvider) activeModel() string { return p.model }

func (p *anthropicProvider) Stream(ctx context.Context, system string, msgs []Msg) (<-chan Chunk, error) {
	return p.StreamOpts(ctx, system, msgs, Options{})
}

func (p *anthropicProvider) StreamOpts(ctx context.Context, system string, msgs []Msg, o Options) (<-chan Chunk, error) {
	payload := map[string]any{
		"model":      p.model,
		"max_tokens": anthropicMaxTokens,
		"stream":     true,
		"messages":   anthropicMessages(msgs),
	}
	if o.Temperature != nil {
		payload["temperature"] = *o.Temperature
	}
	if system != "" {
		payload["system"] = system
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	logging.Trace("ai: anthropic stream", "model", p.model, "endpoint", p.endpoint,
		"hasKey", p.apiKey != "", "msgs", len(msgs), "maxTokens", anthropicMaxTokens,
		"bytes", len(body), "payload", logging.Body(string(body)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	return streamSSE(ctx, p.client, req, extractAnthropicDelta, p.Name(), p.model)
}

// anthropicMessages maps turns to the Anthropic schema; the system prompt is
// passed out-of-band (the top-level system field), so system turns are dropped.
func anthropicMessages(msgs []Msg) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == RoleSystem {
			continue
		}
		out = append(out, map[string]any{"role": string(m.Role), "content": m.Content})
	}
	return out
}

func extractAnthropicDelta(data []byte) (string, bool, error) {
	var d struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return "", false, nil
	}
	switch d.Type {
	case "content_block_delta":
		if d.Delta.Type == "text_delta" {
			return d.Delta.Text, false, nil
		}
	case "message_stop":
		return "", true, nil
	case "error":
		msg := "stream error"
		if d.Error != nil && d.Error.Message != "" {
			msg = d.Error.Message
		}
		return "", false, fmt.Errorf("anthropic stream error: %s", msg)
	}
	return "", false, nil
}
