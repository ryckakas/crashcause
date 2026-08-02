package ai

import "context"

// ollamaProvider talks to a local or in-cluster ollama server. It needs no
// API key and, when pointed at localhost or an in-cluster service, no crash
// data leaves the operator's machine or cluster — the recommended option for
// privacy-sensitive environments (spec §6).
type ollamaProvider struct {
	client *client
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	NumPredict int `json:"num_predict"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages []ollamaMessage `json:"messages"`
	Options  ollamaOptions   `json:"options"`
}

type ollamaResponse struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

// Name implements Provider.
func (p *ollamaProvider) Name() string { return ProviderOllama }

// Summarize implements Provider.
//
// CONTRACT: req must already be redacted. Summarizer.Summarize is the
// supported entry point and guarantees that; a caller invoking a Provider
// directly is responsible for redacting first.
func (p *ollamaProvider) Summarize(ctx context.Context, req Request) (string, error) {
	payload := ollamaRequest{
		Model:  p.client.model,
		Stream: false,
		Messages: []ollamaMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: buildUserPrompt(req)},
		},
		Options: ollamaOptions{NumPredict: p.client.maxTokens},
	}

	var resp ollamaResponse
	if err := p.client.postJSON(ctx, "/api/chat", nil, payload, &resp); err != nil {
		return "", err
	}
	return finalizeSummary(p.client.name, resp.Message.Content)
}
