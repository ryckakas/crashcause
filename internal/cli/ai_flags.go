package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/ryckakas/crashcause/internal/ai"
)

// apiKeyEnv is the only place the AI API key is ever read from — never a
// flag (ps leakage) and never logged.
const apiKeyEnv = "CRASHCAUSE_AI_API_KEY"

// aiOptions holds the AI-summary flags shared by inspect and watch. Values
// are bound directly by addAIFlags so both commands stay consistent.
type aiOptions struct {
	enabled     bool
	provider    string
	url         string
	model       string
	timeout     time.Duration
	redact      bool
	redactIPs   bool
	redactExtra string
}

// addAIFlags registers the AI-summary flags on fs, shared verbatim between
// the inspect and watch commands.
func (o *aiOptions) addAIFlags(fs *pflag.FlagSet) {
	fs.BoolVar(&o.enabled, "ai", false, "generate an AI natural-language summary of the diagnosis")
	fs.StringVar(&o.provider, "ai-provider", "anthropic", "AI provider to use: anthropic|openai|ollama")
	fs.StringVar(&o.url, "ai-url", "", "override base URL for the AI provider (e.g. for a local ollama instance)")
	fs.StringVar(&o.model, "ai-model", "", "model to use; empty means the provider's default (anthropic: claude-haiku-4-5, openai: gpt-4o-mini, ollama: llama3.1)")
	fs.DurationVar(&o.timeout, "ai-timeout", ai.DefaultTimeout, "per-summary timeout; raise it for a self-hosted model that must load weights on its first call")
	fs.BoolVar(&o.redact, "ai-redact", true, "redact likely-sensitive values (secrets, tokens) from evidence before sending it to the AI provider")
	fs.BoolVar(&o.redactIPs, "ai-redact-ips", false, "additionally redact IP addresses from evidence before sending it to the AI provider")
	fs.StringVar(&o.redactExtra, "ai-redact-extra", "", "path to a file of extra regex patterns to redact from evidence before sending it to the AI provider")
}

// firstUseNotice is the honesty requirement from the spec: printed to stderr
// once per invocation whenever AI summarization is enabled.
const firstUseNotice = "crashcause: AI summarization is enabled. Redaction is best-effort pattern " +
	"matching, NOT a guarantee — do not enable AI on workloads whose logs may " +
	"contain secrets you cannot afford to send. See the docs for the full pattern list."

// buildSummarizer turns the parsed AI flags into a ready ai.Summarizer, or
// (nil, nil) when AI is disabled. The API key comes exclusively from the
// CRASHCAUSE_AI_API_KEY environment variable.
func (o *aiOptions) buildSummarizer(errOut io.Writer) (*ai.Summarizer, error) {
	if !o.enabled {
		return nil, nil
	}

	var redactor *ai.Redactor
	if o.redact {
		extra, err := readRedactPatterns(o.redactExtra)
		if err != nil {
			return nil, err
		}
		redactor, err = ai.NewRedactor(ai.RedactorOptions{
			RedactIPs:     o.redactIPs,
			ExtraPatterns: extra,
		})
		if err != nil {
			return nil, fmt.Errorf("building redactor: %w", err)
		}
	}

	provider, err := ai.New(ai.Config{
		Provider: o.provider,
		APIKey:   os.Getenv(apiKeyEnv),
		BaseURL:  o.url,
		Model:    o.model,
		Timeout:  o.timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("configuring AI provider: %w", err)
	}

	_, _ = fmt.Fprintln(errOut, firstUseNotice)
	return ai.NewSummarizerWithTimeout(provider, redactor, o.timeout), nil
}

// readRedactPatterns loads one regex per line from path, skipping blank
// lines and #-comments. An empty path returns no patterns.
func readRedactPatterns(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("reading --ai-redact-extra file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var patterns []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading --ai-redact-extra file: %w", err)
	}
	return patterns, nil
}
