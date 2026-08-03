package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func patternFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "patterns.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing pattern file: %v", err)
	}
	return path
}

func TestBuildSummarizerDisabled(t *testing.T) {
	var stderr bytes.Buffer
	opts := &aiOptions{enabled: false}

	s, err := opts.buildSummarizer(&stderr)
	if err != nil {
		t.Fatalf("buildSummarizer() error = %v, want nil", err)
	}
	if s != nil {
		t.Errorf("buildSummarizer() = %v, want nil when --ai is off", s)
	}
	if stderr.Len() != 0 {
		t.Errorf("notice printed while AI is disabled: %q", stderr.String())
	}
}

func TestBuildSummarizerRedactDisabledSkipsRedactor(t *testing.T) {
	t.Setenv(apiKeyEnv, "test-key-not-a-real-credential")
	var stderr bytes.Buffer
	opts := &aiOptions{
		enabled:  true,
		provider: "anthropic",
		redact:   false,
		// A missing extra-patterns file must be ignored when redaction is
		// off, proving the redactor path is skipped entirely.
		redactExtra: filepath.Join(t.TempDir(), "does-not-exist.txt"),
	}

	s, err := opts.buildSummarizer(&stderr)
	if err != nil {
		t.Fatalf("buildSummarizer() error = %v, want nil", err)
	}
	if s == nil || s.Provider() == nil {
		t.Fatal("buildSummarizer() returned no summarizer/provider")
	}
	if got := s.Provider().Name(); got != "anthropic" {
		t.Errorf("provider name = %q, want anthropic", got)
	}
	if !strings.Contains(stderr.String(), "AI summarization is enabled") {
		t.Errorf("first-use notice not printed, stderr = %q", stderr.String())
	}
}

func TestBuildSummarizerMissingExtraPatternsFile(t *testing.T) {
	var stderr bytes.Buffer
	opts := &aiOptions{
		enabled:     true,
		provider:    "anthropic",
		redact:      true,
		redactExtra: filepath.Join(t.TempDir(), "does-not-exist.txt"),
	}

	s, err := opts.buildSummarizer(&stderr)
	if err == nil {
		t.Fatalf("expected an error for a missing patterns file, got summarizer %v", s)
	}
	if !strings.Contains(err.Error(), "--ai-redact-extra") {
		t.Errorf("error should name the flag, got %q", err.Error())
	}
	if stderr.Len() != 0 {
		t.Errorf("notice printed despite failure: %q", stderr.String())
	}
}

func TestBuildSummarizerInvalidExtraPattern(t *testing.T) {
	var stderr bytes.Buffer
	opts := &aiOptions{
		enabled:     true,
		provider:    "anthropic",
		redact:      true,
		redactExtra: patternFile(t, "# comment\n\n[unclosed\n"),
	}

	s, err := opts.buildSummarizer(&stderr)
	if err == nil {
		t.Fatalf("expected an error for an invalid regex, got summarizer %v", s)
	}
	if !strings.Contains(err.Error(), "building redactor") {
		t.Errorf("error should come from the redactor, got %q", err.Error())
	}
}

func TestReadRedactPatterns(t *testing.T) {
	patterns, err := readRedactPatterns("")
	if err != nil || patterns != nil {
		t.Errorf("readRedactPatterns(\"\") = %v, %v, want nil, nil", patterns, err)
	}

	path := patternFile(t, "# header comment\npassword=\\S+\n\n  token-[0-9]+  \n")
	patterns, err = readRedactPatterns(path)
	if err != nil {
		t.Fatalf("readRedactPatterns() error = %v", err)
	}
	want := []string{`password=\S+`, `token-[0-9]+`}
	if len(patterns) != len(want) {
		t.Fatalf("readRedactPatterns() = %v, want %v", patterns, want)
	}
	for i := range want {
		if patterns[i] != want[i] {
			t.Errorf("pattern[%d] = %q, want %q", i, patterns[i], want[i])
		}
	}
}
