// Package inspect implements the one-shot `crashcause inspect` mode: resolve a
// pod, collect its inputs, run the pure classification engine over every
// container that warrants diagnosis, optionally enrich the primary diagnosis
// with an AI summary, and render a report.
//
// The package is deliberately free of cobra (and of any flag parsing): Run
// takes a fully-resolved Options value and a kubernetes.Interface, so the whole
// mode can be driven end-to-end in tests with a fake clientset. It returns the
// process exit code rather than an error because the exit code is part of the
// tool's contract (spec §5.1) and does not map onto Go's error semantics:
// "nothing to diagnose" is a successful run that must exit 2.
package inspect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/ryckakas/crashcause/internal/ai"
	"github.com/ryckakas/crashcause/internal/collect"
	"github.com/ryckakas/crashcause/internal/engine"
)

// Exit codes, per spec §5.1. They are the CLI's external contract.
const (
	// ExitDiagnosed means at least one diagnosis was produced and rendered.
	ExitDiagnosed = 0
	// ExitError means the inspection itself failed (pod not found, no such
	// container, API error, bad options).
	ExitError = 1
	// ExitNothingToDiagnose means the run succeeded but the pod is not
	// crashing — deliberately distinct from both success and failure so that
	// scripts can tell "healthy" from "broken tooling".
	ExitNothingToDiagnose = 2
)

// Output formats accepted by Options.Output.
const (
	OutputHuman = "human"
	OutputJSON  = "json"
)

// defaultNamespace is used when neither the flag nor the kubeconfig context
// supplies one.
const defaultNamespace = "default"

// Options is everything Run needs. Every field is already resolved: the CLI
// layer owns flag parsing, kubeconfig loading and namespace resolution.
type Options struct {
	Namespace string
	Pod       string
	// Container restricts the diagnosis to a single container. Empty means
	// "every container of the pod that warrants diagnosis".
	Container string
	// PreviousLines is the number of previous-log lines to tail per container.
	// Values <= 0 fall back to the collector default.
	PreviousLines int64
	// InitStuckThreshold is how long an init container may run before it is
	// reported as stuck. Values <= 0 fall back to the collector default.
	InitStuckThreshold time.Duration
	// Output is "human" (default) or "json".
	Output string
	// Verbose reports every rule that matched, not just the primary diagnosis.
	Verbose bool
	// Summarizer enables the AI layer. A nil Summarizer means AI is off, which
	// is the default and the only state reachable without --ai.
	Summarizer *ai.Summarizer
	// Server is the API server URL the client talks to, used only to name the
	// cluster in connectivity and credential failure messages. Empty is fine.
	Server string

	Out    io.Writer
	ErrOut io.Writer
}

// containerResult pairs the inputs collected for one container with the
// diagnoses the engine produced from them. The inputs are retained because the
// AI layer needs that container's log tail and image.
type containerResult struct {
	inputs    engine.Inputs
	diagnoses []engine.Diagnosis
}

// primary is the diagnosis rendered by default (engine.Classify returns a
// priority-ordered slice whose index 0 is the primary).
func (r *containerResult) primary() *engine.Diagnosis {
	if len(r.diagnoses) == 0 {
		return nil
	}
	return &r.diagnoses[0]
}

// Run performs the whole one-shot inspection and returns the process exit code.
//
// It never panics on a missing pod, an unknown container or an unreachable API
// server: those are reported as a single-line message on ErrOut and exit 1.
func Run(ctx context.Context, client kubernetes.Interface, opts Options) int {
	opts = opts.withDefaults()

	if err := opts.validate(); err != nil {
		return opts.fail(err)
	}

	inputs, err := opts.collectInputs(ctx, client)
	if err != nil {
		return opts.fail(err)
	}

	results := classify(inputs)
	if len(results) == 0 {
		return opts.reportNothingToDiagnose(inputs)
	}

	opts.attachAISummaries(ctx, results)

	if err := opts.render(results); err != nil {
		return opts.fail(err)
	}
	return ExitDiagnosed
}

// withDefaults fills in the fields a caller may legitimately leave zero.
func (o Options) withDefaults() Options {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.ErrOut == nil {
		o.ErrOut = os.Stderr
	}
	if o.Output == "" {
		o.Output = OutputHuman
	}
	if o.Namespace == "" {
		o.Namespace = defaultNamespace
	}
	return o
}

// validate rejects option combinations that cannot produce a report.
func (o Options) validate() error {
	if o.Pod == "" {
		return errors.New("no pod name given")
	}
	switch o.Output {
	case OutputHuman, OutputJSON:
	default:
		return fmt.Errorf("invalid --output %q: must be one of human|json", o.Output)
	}
	return nil
}

// fail prints a one-line error on ErrOut and returns the error exit code. It is
// the single place where inspect-mode failures are rendered, so the CLI layer
// can stay silent about anything Run already reported.
func (o Options) fail(err error) int {
	_, _ = fmt.Fprintln(o.ErrOut, "error:", err)
	return ExitError
}

// collectInputs builds a collector configured for inspect mode and gathers the
// engine inputs for the pod.
//
// Inspect runs with the invoking user's own credentials against a single pod,
// so it deliberately uses no log rate limiter (spec §8: the limiter exists to
// stop watch mode from DoSing the API server during a crash storm).
func (o Options) collectInputs(ctx context.Context, client kubernetes.Interface) ([]engine.Inputs, error) {
	copts := collect.DefaultOptions()
	copts.CollectLogs = true
	copts.CollectNode = true
	copts.LogRateLimiter = nil
	if o.PreviousLines > 0 {
		copts.PreviousLogLines = o.PreviousLines
	}
	if o.InitStuckThreshold > 0 {
		copts.InitStuckThreshold = o.InitStuckThreshold
	}

	inputs, err := collect.New(client, copts).ForPod(ctx, o.Namespace, o.Pod, o.Container)
	if err != nil {
		return nil, o.describeCollectError(err)
	}
	return inputs, nil
}

// describeCollectError turns a collector error into a short, actionable line.
// The wrapped API error is preserved for anything we do not recognize.
func (o Options) describeCollectError(err error) error {
	switch {
	case errors.Is(err, collect.ErrContainerNotFound):
		return fmt.Errorf("pod %s/%s has no container %q", o.Namespace, o.Pod, o.Container)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("pod %s/%s not found", o.Namespace, o.Pod)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("not allowed to read pod %s/%s: %w", o.Namespace, o.Pod, err)
	default:
		return explainClusterFailure(err, o.Server)
	}
}

// classify runs the engine over every collected container and keeps only the
// containers that produced at least one diagnosis. An empty engine result means
// "normal lifecycle, nothing wrong" and must not be reported as a finding.
func classify(inputs []engine.Inputs) []containerResult {
	var out []containerResult
	for _, in := range inputs {
		ds := engine.Classify(in)
		if len(ds) == 0 {
			continue
		}
		out = append(out, containerResult{inputs: in, diagnoses: ds})
	}
	return out
}

// reportNothingToDiagnose renders the exit-2 case: the pod exists and was
// inspected successfully, but nothing about it is wrong.
func (o Options) reportNothingToDiagnose(inputs []engine.Inputs) int {
	msg := fmt.Sprintf("pod %s/%s: nothing to diagnose (pod is not crashing)", o.Namespace, o.Pod)

	// In JSON mode stdout must stay machine-readable: emit the same document
	// shape with an empty diagnoses array and put the prose on stderr.
	w := o.Out
	if o.Output == OutputJSON {
		w = o.ErrOut
		if err := renderJSON(o.Out, o.Namespace, o.Pod, nil, o.Verbose); err != nil {
			return o.fail(err)
		}
	}

	_, _ = fmt.Fprintln(w, msg)
	if o.Verbose {
		_, _ = fmt.Fprintln(w, checkedSummary(inputs))
	}
	return ExitNothingToDiagnose
}

// checkedSummary describes what was examined, so that a --verbose exit-2 run
// still tells the user the tool looked at the right thing.
func checkedSummary(inputs []engine.Inputs) string {
	if len(inputs) == 0 {
		return "checked: no container reported a crash, restart, or blocking wait state"
	}
	names := make([]string, 0, len(inputs))
	for _, in := range inputs {
		names = append(names, fmt.Sprintf("%s (%s)", in.Container, in.Kind))
	}
	return "checked: " + strings.Join(names, ", ") + " — no rule matched"
}

// aiCauses is the exact set of primary causes the AI layer is allowed to
// summarize (spec §6): rule-based causes are already explained by the rules,
// and only an application-level crash has a stack trace worth interpreting.
var aiCauses = map[engine.CauseCode]bool{
	engine.CauseAppExitNonzero: true,
	engine.CauseUnknown:        true,
}

// attachAISummaries fills in Diagnosis.AISummary for every container whose
// PRIMARY diagnosis is AI-eligible.
//
// Failure semantics (spec §6): an AI error never changes the exit code and
// never drops a diagnosis. It produces a single notice line on ErrOut; repeated
// identical notices (one per container) are collapsed so a multi-container pod
// with an unreachable provider does not spam stderr.
func (o Options) attachAISummaries(ctx context.Context, results []containerResult) {
	if o.Summarizer == nil {
		return
	}
	seen := map[string]bool{}
	for i := range results {
		d := results[i].primary()
		if d == nil || !aiCauses[d.Cause] {
			continue
		}
		req := ai.Request{
			Cause:    string(d.Cause),
			Evidence: d.Evidence,
			LogTail:  results[i].inputs.LogTail,
			Image:    results[i].inputs.Image,
		}
		summary, err := o.Summarizer.Summarize(ctx, req)
		if err != nil {
			notice := aiNotice(err)
			if !seen[notice] {
				seen[notice] = true
				_, _ = fmt.Fprintln(o.ErrOut, notice)
			}
			continue
		}
		summary = strings.TrimSpace(summary)
		if summary == "" {
			continue
		}
		d.AISummary = &summary
	}
}

// aiNotice formats the one-line AI failure notice, avoiding a doubled prefix
// when the summarizer already wrapped the provider error with its own.
func aiNotice(err error) string {
	const prefix = "AI summary unavailable: "
	msg := err.Error()
	if trimmed, ok := cutPrefixFold(msg, prefix); ok {
		msg = trimmed
	}
	return prefix + msg
}

// cutPrefixFold is strings.CutPrefix with ASCII case folding.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// render writes the report in the configured output format.
func (o Options) render(results []containerResult) error {
	if o.Output == OutputJSON {
		return renderJSON(o.Out, o.Namespace, o.Pod, results, o.Verbose)
	}
	return renderHuman(o.Out, results, o.Verbose)
}
