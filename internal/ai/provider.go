// Package ai defines the contract for optional AI-assisted crash summaries.
//
// Providers (anthropic, openai, ollama, ...) are plain HTTP clients — no
// vendor SDKs are used. Each provider is bring-your-own-key: the API key is
// read from the CRASHCAUSE_AI_API_KEY environment variable, never from a
// flag or config file. AI is strictly additive: a failure to reach a
// provider, a timeout, or a malformed response must never cause a
// Diagnosis to fail or be withheld — it only means AISummary stays nil.
package ai

import "context"

// Request carries the information a Provider needs to produce a natural
// language summary of a diagnosed crash.
type Request struct {
	Cause    string
	Evidence []string
	LogTail  []string
	Image    string
}

// Provider is implemented by anthropic/openai/ollama clients.
type Provider interface {
	Name() string
	Summarize(ctx context.Context, req Request) (string, error)
}
