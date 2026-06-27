package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
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

func (p *anthropicProvider) Stream(ctx context.Context, system string, msgs []Msg) (<-chan Chunk, error) {
	payload := map[string]any{
		"model":      p.model,
		"max_tokens": anthropicMaxTokens,
		"stream":     true,
		"messages":   anthropicMessages(msgs),
	}
	if system != "" {
		payload["system"] = system
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	return streamSSE(ctx, p.client, req, extractAnthropicDelta)
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

func extractAnthropicDelta(data []byte) (string, bool) {
	var d struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return "", false
	}
	switch d.Type {
	case "content_block_delta":
		if d.Delta.Type == "text_delta" {
			return d.Delta.Text, false
		}
	case "message_stop":
		return "", true
	}
	return "", false
}
