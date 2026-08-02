package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ryckakas/crashcause/internal/engine"
)

// Report is the stable JSON document emitted by `--output json`: one document
// per pod, covering every diagnosed container. Field names and the nested
// Diagnosis encoding are part of the tool's external contract.
type Report struct {
	Pod       string      `json:"pod"`
	Namespace string      `json:"namespace"`
	Diagnoses []Diagnosis `json:"diagnoses"`
}

// Diagnosis is the JSON projection of an engine.Diagnosis. It exists solely to
// make ai_summary a NULLABLE field rather than an absent one: engine.Diagnosis
// tags it `omitempty`, so a consumer of the raw type sees the key vanish when
// no summary was generated. The documented contract is `"ai_summary": null`, so
// the shadowing field (depth 0 beats the embedded one at depth 1) restores it
// without modifying the engine's shared type.
type Diagnosis struct {
	engine.Diagnosis
	AISummary *string `json:"ai_summary"`
}

// newDiagnosis projects an engine diagnosis into its JSON form.
func newDiagnosis(d engine.Diagnosis) Diagnosis {
	return Diagnosis{Diagnosis: d, AISummary: d.AISummary}
}

// Indentation used by the human renderer. Deliberately plain: no ANSI color,
// no box drawing, no external color library — the report is meant to survive
// being piped into a ticket, a chat message or a CI log unchanged, and NO_COLOR
// handling you never need is one less thing to get wrong.
const (
	bodyIndent   = "  "
	bulletIndent = "    "
)

// renderHuman writes the compact, evidence-first report (spec §5.1). Containers
// are rendered in collection order; each container contributes one block.
func renderHuman(w io.Writer, results []containerResult, verbose bool) error {
	bw := &errWriter{w: w}
	for i := range results {
		if i > 0 {
			bw.line("")
		}
		renderContainer(bw, results[i], verbose)
	}
	return bw.err
}

// renderContainer writes one container's block: header, explanation, evidence,
// next steps, optional AI summary, optional secondary matches.
func renderContainer(bw *errWriter, res containerResult, verbose bool) {
	d := res.primary()
	if d == nil {
		return
	}

	bw.linef("%s/%s container %s — %s (%s confidence)", d.Namespace, d.Pod, d.Container, d.Cause, d.Confidence)

	if d.Explanation != "" {
		bw.line("")
		bw.indented(bodyIndent, d.Explanation)
	}

	if len(d.Evidence) > 0 {
		bw.line("")
		bw.linef("%sEvidence:", bodyIndent)
		for _, e := range d.Evidence {
			bw.bullet(e)
		}
	}

	if len(d.NextSteps) > 0 {
		bw.line("")
		bw.linef("%sNext steps:", bodyIndent)
		for _, s := range d.NextSteps {
			bw.bullet(s)
		}
	}

	if d.AISummary != nil && *d.AISummary != "" {
		bw.line("")
		bw.linef("%sAI summary (generated):", bodyIndent)
		bw.indented(bulletIndent, *d.AISummary)
	}

	if verbose && len(res.diagnoses) > 1 {
		bw.line("")
		for _, sec := range res.diagnoses[1:] {
			bw.linef("%salso matched: %s (%s) — %s", bodyIndent, sec.Cause, sec.Confidence, oneLine(sec.Explanation))
		}
	}
}

// renderJSON writes the machine-readable report. Which diagnoses are included
// follows the human renderer exactly: the primary per container by default, and
// every match under --verbose — so a script and a human running the same
// command never see different findings.
func renderJSON(w io.Writer, namespace, pod string, results []containerResult, verbose bool) error {
	report := Report{
		Pod:       pod,
		Namespace: namespace,
		// Never nil: an empty diagnoses array encodes as [] rather than null,
		// which keeps `jq '.diagnoses | length'` working on a healthy pod.
		Diagnoses: []Diagnosis{},
	}
	for i := range results {
		if verbose {
			for _, d := range results[i].diagnoses {
				report.Diagnoses = append(report.Diagnoses, newDiagnosis(d))
			}
			continue
		}
		if d := results[i].primary(); d != nil {
			report.Diagnoses = append(report.Diagnoses, newDiagnosis(*d))
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("encoding json report: %w", err)
	}
	return nil
}

// oneLine collapses a multi-line explanation onto a single line, for the
// one-line-per-match secondary listing.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// errWriter accumulates the first write error so the renderer can stay free of
// per-line error handling; a broken stdout is reported once, at the end.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) line(s string) {
	if e.err != nil {
		return
	}
	if _, err := fmt.Fprintln(e.w, s); err != nil {
		e.err = err
	}
}

func (e *errWriter) linef(format string, args ...any) {
	e.line(fmt.Sprintf(format, args...))
}

// bullet writes one evidence / next-step item, indenting continuation lines so
// multi-line evidence (a stack trace snippet) stays visually attached.
func (e *errWriter) bullet(s string) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	e.linef("%s- %s", bulletIndent, lines[0])
	for _, l := range lines[1:] {
		e.linef("%s  %s", bulletIndent, l)
	}
}

// indented writes a possibly multi-line paragraph with a fixed indent.
func (e *errWriter) indented(indent, s string) {
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		e.linef("%s%s", indent, l)
	}
}
