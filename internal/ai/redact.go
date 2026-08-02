package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// Redaction placeholders. These are deliberately loud so that a human reading
// an AI summary can tell that something was removed from the input.
const (
	redactedPlaceholder   = "[REDACTED]"
	redactedIPPlaceholder = "[REDACTED-IP]"
)

// Built-in patterns. Evaluation order is significant and is documented on
// Redact: the URL-credential rule runs before the generic key=value rule so
// that a connection string keeps its host and port (they are diagnosis, not
// secret) instead of being flattened to [REDACTED] by the generic rule.
var (
	// scheme://user:pass@host:port/... — only the userinfo is consumed, so
	// everything from the host onwards survives untouched.
	urlCredentialsRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/@:]+:[^\s/@]*@`)

	// Authorization: Bearer <tok> / Authorization: Basic <b64> and friends
	// (also Proxy-Authorization, WWW-Authorization). The header name and the
	// auth scheme survive; only the credential is scrubbed.
	authHeaderRe = regexp.MustCompile(`(?i)\b((?:[a-z]+-)?authorization)("?\s*[:=]\s*)("[^"]*"|'[^']*'|(?:bearer|basic|digest|token)\s+[^\s,;"']+|[^\s,;"']+)`)
	authSchemeRe = regexp.MustCompile(`(?i)^(bearer|basic|digest|token)(\s+)\S.*$`)

	// AWS secret access keys: a 40-char base64-ish blob is only scrubbed when
	// it is assigned to an aws-secret-ish key name, so that random hashes,
	// image digests and build IDs are not destroyed.
	awsSecretRe = regexp.MustCompile(`(?i)\b(aws[_-]?secret[_-]?(?:access[_-]?)?key)("?\s*[:=]\s*)("?)[A-Za-z0-9/+=]{40}("?)`)

	// AWS access key IDs.
	awsAccessKeyIDRe = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)

	// key=value / key: value where the key ENDS WITH a sensitive word. The
	// "ends with" anchoring is what keeps `secretName: db-creds` and
	// `tokenizer: bpe` (diagnostic) out of the redactor while still catching
	// DB_PASSWORD=..., x-api-key: ... and aws_secret_access_key=...
	keyValueRe = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.\-]*(?:api[_-]?key|apikey|password|passwd|pwd|secret|access[_-]?key|token|auth))("?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)

	// JWT-shaped strings.
	jwtRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)

	// Bare IPv4 addresses — opt-in only. Any :port is left alone because the
	// port is part of the diagnosis ("connection refused to ...:5432").
	ipv4Re = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\b`)

	urlSchemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://`)
)

// RedactorOptions configures a Redactor.
type RedactorOptions struct {
	// RedactIPs replaces bare IPv4 addresses with [REDACTED-IP]. Default
	// false: "connection refused to 10.2.3.4:5432" is frequently THE
	// diagnostic fact, so IPs are kept unless an operator opts in
	// (--ai-redact-ips) for a stricter environment.
	RedactIPs bool

	// ExtraPatterns are org-specific regexes (Go/RE2 syntax), one per entry,
	// sourced from the --ai-redact-extra file; the caller reads the file and
	// passes the lines. Blank lines and lines starting with '#' are ignored so
	// that the file can be commented. Every match is replaced with [REDACTED].
	ExtraPatterns []string
}

// Redactor performs best-effort, enumerated scrubbing of well-known secret
// shapes before crash evidence and log tails are sent to an AI provider.
//
// Best-effort is meant literally and is a product decision, not a caveat:
// redaction is pattern matching over unstructured application logs, NOT a
// guarantee. The exact pattern list is published (see Patterns) so that the
// behavior is auditable rather than merely asserted. The primary control on
// what leaves a cluster is the per-namespace AI allowlist; redaction is
// defense in depth.
//
// A nil *Redactor is safe to call and behaves like a zero-value one: the
// built-in patterns apply, IPs are kept, no extra patterns. Disabling
// redaction entirely (--ai-redact=false) is modeled by passing a nil
// Redactor to NewSummarizer, which then skips redaction altogether.
type Redactor struct {
	redactIPs bool
	extra     []*regexp.Regexp
	extraSrc  []string
}

// NewRedactor builds a Redactor. It returns an error if any extra pattern is
// not a valid regular expression, so that a typo in the operator's
// --ai-redact-extra file fails loudly at startup instead of silently not
// redacting anything.
func NewRedactor(opts RedactorOptions) (*Redactor, error) {
	r := &Redactor{redactIPs: opts.RedactIPs}
	for _, raw := range opts.ExtraPatterns {
		pattern := strings.TrimSpace(raw)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("ai: invalid extra redaction pattern %q: %w", pattern, err)
		}
		r.extra = append(r.extra, re)
		r.extraSrc = append(r.extraSrc, pattern)
	}
	return r, nil
}

// Redact returns s with every built-in and extra pattern applied.
//
// Rules run in a fixed order:
//  1. credentials in URLs (host and port preserved)
//  2. Authorization headers (scheme preserved)
//  3. AWS secret access keys
//  4. generic key=value / key: value secrets (URL values deferred to rule 1)
//  5. AWS access key IDs
//  6. JWT-shaped strings
//  7. operator-supplied extra patterns
//  8. bare IPv4 addresses (only when RedactIPs is set)
//
// Stack traces, file paths, hostnames, exception names, exit codes and port
// numbers are intentionally left untouched — destroying them would destroy
// the diagnosis the summary is supposed to explain.
func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	out := urlCredentialsRe.ReplaceAllString(s, "${1}***@")
	out = authHeaderRe.ReplaceAllStringFunc(out, redactAuthHeaderMatch)
	out = awsSecretRe.ReplaceAllString(out, "${1}${2}${3}"+redactedPlaceholder+"${4}")
	out = keyValueRe.ReplaceAllStringFunc(out, redactKeyValueMatch)
	out = awsAccessKeyIDRe.ReplaceAllString(out, redactedPlaceholder)
	out = jwtRe.ReplaceAllString(out, redactedPlaceholder)
	if r == nil {
		return out
	}
	for _, re := range r.extra {
		out = re.ReplaceAllString(out, redactedPlaceholder)
	}
	if r.redactIPs {
		out = ipv4Re.ReplaceAllString(out, redactedIPPlaceholder)
	}
	return out
}

// RedactLines applies Redact to every line, returning a new slice. A nil input
// yields a nil result.
func (r *Redactor) RedactLines(lines []string) []string {
	if lines == nil {
		return nil
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = r.Redact(l)
	}
	return out
}

// Patterns returns a human-readable description of every pattern this
// Redactor applies, in evaluation order. It is published in the docs and is
// printed by the CLI: auditability over assurance (spec §6 / decision 6).
func (r *Redactor) Patterns() []string {
	patterns := []string{
		"credentials in URLs: `scheme://user:pass@host:port/path` becomes `scheme://***@host:port/path` — the host and port are KEPT, they are diagnosis, not secret",
		"Authorization headers (case-insensitive, including Proxy-/WWW- prefixed forms): the credential after `Authorization:` or `Authorization=` is replaced with " + redactedPlaceholder + "; the header name and the auth scheme (Bearer/Basic/Digest/Token) are kept",
		"AWS secret access keys: a 40-character base64-ish value assigned to an `aws_secret_access_key`-style key name (only in that position, so random hashes and image digests survive)",
		"key=value and key: value pairs whose key ends with api_key/apikey/password/passwd/pwd/secret/access_key/token/auth (case-insensitive, any prefix such as DB_ or x-): the value — a quoted string or a run of non-whitespace — is replaced with " + redactedPlaceholder + "; values that are themselves URLs are left to the URL rule above so the host and port survive",
		"AWS access key IDs: `AKIA` followed by 16 uppercase alphanumerics",
		"JWT-shaped strings: `eyJ<base64url>.<base64url>.<base64url>`",
	}
	if r != nil {
		for _, src := range r.extraSrc {
			patterns = append(patterns, "operator-supplied extra pattern (--ai-redact-extra): "+src)
		}
	}
	if r != nil && r.redactIPs {
		patterns = append(patterns, "bare IPv4 addresses are replaced with "+redactedIPPlaceholder+" (--ai-redact-ips is on); any `:port` still survives")
	} else {
		patterns = append(patterns, "bare IPv4 addresses are NOT redacted by default — \"connection refused to 10.2.3.4:5432\" is often the diagnosis itself; opt in with --ai-redact-ips")
	}
	return patterns
}

func redactAuthHeaderMatch(match string) string {
	sub := authHeaderRe.FindStringSubmatch(match)
	if sub == nil {
		return match
	}
	return sub[1] + sub[2] + scrubAuthValue(sub[3])
}

// scrubAuthValue replaces the credential but keeps the auth scheme, which is
// diagnostically useful (Basic vs Bearer says a lot) and is not a secret.
func scrubAuthValue(value string) string {
	if inner, quote, ok := unquote(value); ok {
		return quote + scrubAuthValue(inner) + quote
	}
	if s := authSchemeRe.FindStringSubmatch(value); s != nil {
		return s[1] + s[2] + redactedPlaceholder
	}
	return redactedPlaceholder
}

func redactKeyValueMatch(match string) string {
	sub := keyValueRe.FindStringSubmatch(match)
	if sub == nil {
		return match
	}
	key, sep, value := sub[1], sub[2], sub[3]
	if inner, _, ok := unquote(value); ok {
		if urlSchemeRe.MatchString(inner) {
			return match
		}
	} else if urlSchemeRe.MatchString(value) {
		// Already handled by the URL-credential rule, which preserves the
		// host and port. Flattening it here would throw that away.
		return match
	}
	return key + sep + scrubValue(value)
}

func scrubValue(value string) string {
	if _, quote, ok := unquote(value); ok {
		return quote + redactedPlaceholder + quote
	}
	return redactedPlaceholder
}

// unquote splits a "..." or '...' value into its inner text and quote
// character. ok is false when the value is not quoted.
func unquote(value string) (inner, quote string, ok bool) {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1], string(value[0]), true
	}
	return value, "", false
}
