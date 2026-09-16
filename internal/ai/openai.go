package ai

import (
	"context"
	"net/http"
)

type openAIProvider struct {
	client *client
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_completion_tokens"`
	Messages  []openAIMessage `json:"messages"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (p *openAIProvider) Name() string { return ProviderOpenAI }

// CONTRACT: req must already be redacted. Summarizer.Summarize is the
// supported entry point and guarantees that; a caller invoking a Provider
// directly is responsible for redacting first.
func (p *openAIProvider) Summarize(ctx context.Context, req Request) (string, error) {
	payload := openAIRequest{
		Model:     p.client.model,
		MaxTokens: p.client.maxTokens,
		Messages: []openAIMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: buildUserPrompt(req)},
		},
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+p.client.apiKey)

	var resp openAIResponse
	if err := p.client.postJSON(ctx, "/v1/chat/completions", header, payload, &resp); err != nil {
		return "", err
	}
	var text string
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content
	}
	return finalizeSummary(p.client.name, text)
}
