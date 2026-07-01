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

// openAIProvider speaks the OpenAI-compatible Chat Completions API. The same
// implementation serves OpenAI and the LiteLLM proxy (both expose /chat/completions).
type openAIProvider struct {
	client   *http.Client
	xfer     *transferCounter
	endpoint string // base URL including /v1
	apiKey   string
	model    string
}

func newOpenAIProvider(endpoint, apiKey, model string) *openAIProvider {
	xfer := &transferCounter{}
	return &openAIProvider{
		client:   countingClient(120*time.Second, xfer),
		xfer:     xfer,
		endpoint: endpoint,
		apiKey:   apiKey,
		model:    model,
	}
}

func (p *openAIProvider) Name() string { return "openai" }

func (p *openAIProvider) transfer() (in, out int64) { return p.xfer.snapshot() }

func (p *openAIProvider) Stream(ctx context.Context, system string, msgs []Msg) (<-chan Chunk, error) {
	payload := map[string]any{
		"model":    p.model,
		"stream":   true,
		"messages": openAIMessages(system, msgs),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	logging.Trace("ai: openai stream", "model", p.model, "endpoint", p.endpoint,
		"hasKey", p.apiKey != "", "msgs", len(msgs), "bytes", len(body),
		"payload", logging.Body(string(body)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	return streamSSE(ctx, p.client, req, extractOpenAIDelta, p.Name(), p.model)
}

func openAIMessages(system string, msgs []Msg) []map[string]string {
	out := make([]map[string]string, 0, len(msgs)+1)
	if system != "" {
		out = append(out, map[string]string{"role": "system", "content": system})
	}
	for _, m := range msgs {
		out = append(out, map[string]string{"role": string(m.Role), "content": m.Content})
	}
	return out
}

func extractOpenAIDelta(data []byte) (string, bool, error) {
	var d struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return "", false, nil // unparseable SSE line; skip it
	}
	if d.Error != nil && d.Error.Message != "" {
		return "", false, fmt.Errorf("openai stream error: %s", d.Error.Message)
	}
	if len(d.Choices) == 0 {
		return "", false, nil
	}
	return d.Choices[0].Delta.Content, d.Choices[0].FinishReason != nil, nil
}
