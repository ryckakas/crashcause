package ai

import (
	"context"
	"net/http"
)

// anthropicProvider talks to the Anthropic Messages API over plain net/http.
type anthropicProvider struct {
	client *client
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// Name implements Provider.
func (p *anthropicProvider) Name() string { return ProviderAnthropic }

// Summarize implements Provider.
//
// CONTRACT: req must already be redacted. Summarizer.Summarize is the
// supported entry point and guarantees that; a caller invoking a Provider
// directly is responsible for redacting first.
func (p *anthropicProvider) Summarize(ctx context.Context, req Request) (string, error) {
	payload := anthropicRequest{
		Model:     p.client.model,
		MaxTokens: p.client.maxTokens,
		System:    systemPrompt,
		Messages: []anthropicMessage{
			{Role: "user", Content: buildUserPrompt(req)},
		},
	}
	header := http.Header{}
	header.Set("x-api-key", p.client.apiKey)
	header.Set("anthropic-version", anthropicVersion)

	var resp anthropicResponse
	if err := p.client.postJSON(ctx, "/v1/messages", header, payload, &resp); err != nil {
		return "", err
	}
	var text string
	for _, block := range resp.Content {
		if block.Type == "" || block.Type == "text" {
			text = block.Text
			break
		}
	}
	return finalizeSummary(p.client.name, text)
}
