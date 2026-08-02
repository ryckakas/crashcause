package cli

import "github.com/spf13/pflag"

// aiOptions holds the AI-summary flags shared by inspect and watch. Values
// are bound directly by addAIFlags so both commands stay consistent.
type aiOptions struct {
	enabled     bool
	provider    string
	url         string
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
	fs.BoolVar(&o.redact, "ai-redact", true, "redact likely-sensitive values (secrets, tokens) from evidence before sending it to the AI provider")
	fs.BoolVar(&o.redactIPs, "ai-redact-ips", false, "additionally redact IP addresses from evidence before sending it to the AI provider")
	fs.StringVar(&o.redactExtra, "ai-redact-extra", "", "path to a file of extra regex patterns to redact from evidence before sending it to the AI provider")
}
