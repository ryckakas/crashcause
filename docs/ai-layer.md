# The AI layer

The deep-dive behind the README's [AI layer](../README.md#ai-layer-optional-default-off)
summary: providers and key handling, exactly what is and is never sent, how redaction
works and where it deliberately stops, and what happens when a provider call fails.

## What it does

`--ai` / `ai.enabled` turn on an optional layer that sends the log tail and diagnosis
evidence to an LLM for a short (2-4 sentence) natural-language summary — useful mainly
when the rule engine lands on `app_exit_nonzero` or `unknown`, where the "cause" is an
application bug the rules can't interpret further. It is **BYO-key**: you bring your own
provider API key, the tool ships every provider client, and you implement nothing.

## Providers and key handling

Providers: `anthropic`, `openai`, `ollama` (`--ai-provider`). **`ollama` is the recommended
path for privacy-sensitive environments** — it's keyless and self-hosted, so nothing
leaves your machine or cluster. Each provider has a sensible default model
(`claude-haiku-4-5`, `gpt-4o-mini`, `llama3.1`); override it with `--ai-model`, e.g.
`--ai-model llama3.2:3b` to run a smaller local model. Each summary is bounded by
`--ai-timeout` (default 15s) — ample for a hosted API, but raise it for a self-hosted
model that has to load several GB of weights on its first call. The API key, when one is
needed, comes **only** from the `CRASHCAUSE_AI_API_KEY` environment variable — never a
command-line flag (flags leak via `ps`), and never logged. In Helm, the key is mounted
from a Kubernetes Secret you provide (`ai.existingSecret` + `ai.secretKey`, default key
name `api-key`) into that environment variable; the chart never creates the Secret and
never accepts a plaintext key in `values.yaml`.

## What is sent, what is never sent

**What is sent** (only if `--ai` is enabled): the already-truncated log tail, the cause's
evidence strings, and the container image name.
**What is never sent**: environment variables, Secrets, the full pod spec, or node info.

> Redaction (`--ai-redact`, default on) is **best-effort pattern matching, not a
> guarantee**. Do not enable AI on workloads whose logs may contain secrets you cannot
> afford to send to a third-party provider. The per-namespace allowlist
> (`--ai-namespaces` / `ai.namespaces`) is the primary control; redaction is
> defense-in-depth, not the safety mechanism itself. This notice is also printed to
> stderr the first time `--ai` is used in a given invocation.

The allowlist is **fail-closed**, which is what makes it the primary control rather than a
convenience filter: with `--ai` set but `--ai-namespaces` left empty (or `ai.namespaces:
[]` in the chart), AI summarization is inert in every namespace — nothing is sent
anywhere. You must explicitly pass `--ai-namespaces "*"` (or set `ai.namespaces: ["*"]`)
to opt the whole cluster in; there is no "on by default once `--ai`/`ai.enabled` is set"
behavior to accidentally trigger.

## Redaction coverage

What redaction covers: bearer/api-key/password/token `key=value` pairs, AWS-style access
key IDs, JWT-shaped strings, and credentials embedded in a URL (`://user:pass@host`
becomes `://***@host` — the password is scrubbed, but **host and port are deliberately
kept**, because they're diagnosis, not secret). What it deliberately does not cover by
default: bare IP addresses — "connection refused to 10.2.3.4:5432" is often *the*
diagnostic fact, so IPs survive unless you opt in with `--ai-redact-ips`.
`--ai-redact-extra <regex-file>` adds your own organization-specific patterns (one regex
per line, `#`-comments allowed). The exact built-in pattern list is published in code
(`internal/ai/redact.go`) rather than kept secret, on the theory that auditability beats
false assurance.

## Failure behavior

AI failures never fail a diagnosis: on a provider error or timeout you get the rules-only
result plus a single one-line notice on stderr, not a hard failure. The Prometheus sink
ignores AI output entirely — `ai_summary` never becomes a label or influences a metric.
