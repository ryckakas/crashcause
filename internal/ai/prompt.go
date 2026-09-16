package ai

import "strings"

// systemPrompt is shared by every provider so that summaries read the same
// regardless of which backend produced them.
const systemPrompt = "You are a Kubernetes crash analyst. Given a crash cause classification, " +
	"evidence, and the final log lines of the crashed container, explain the application-level " +
	"root cause in 2-4 sentences. Be concrete about what the logs show. No preamble."

// CONTRACT: req is expected to be ALREADY REDACTED — Summarizer.Summarize
// applies the Redactor before it reaches a provider. Nothing in this file
// redacts anything; a caller that talks to a Provider directly is responsible
// for redacting first.
//
// Only what spec §6 permits is ever assembled here: the cause code, the
// evidence strings, the container image name and the (already truncated) log
// tail. Env vars, secrets, the pod spec and node info are not fields of
// Request and can never be sent.
func buildUserPrompt(req Request) string {
	var b strings.Builder

	if cause := strings.TrimSpace(req.Cause); cause != "" {
		b.WriteString("Crash cause classification: ")
		b.WriteString(cause)
		b.WriteString("\n")
	}
	if image := strings.TrimSpace(req.Image); image != "" {
		b.WriteString("Container image: ")
		b.WriteString(image)
		b.WriteString("\n")
	}
	if evidence := nonEmpty(req.Evidence); len(evidence) > 0 {
		b.WriteString("\nEvidence:\n")
		for _, e := range evidence {
			b.WriteString("- ")
			b.WriteString(e)
			b.WriteString("\n")
		}
	}
	if logs := nonEmpty(req.LogTail); len(logs) > 0 {
		b.WriteString("\nFinal log lines of the crashed container:\n")
		b.WriteString(strings.Join(logs, "\n"))
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "No evidence was collected for this crash."
	}
	return strings.TrimRight(b.String(), "\n")
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
