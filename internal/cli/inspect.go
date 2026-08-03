package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	"github.com/ryckakas/crashcause/internal/inspect"
)

// inspectOptions holds all flag-bound settings for the inspect command.
type inspectOptions struct {
	container       string
	output          string
	previousLines   int
	initStuckThresh time.Duration
	verbose         bool
	ai              aiOptions
	configFlags     *genericclioptions.ConfigFlags
}

func newInspectCmd() *cobra.Command {
	opts := &inspectOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
	}

	cmd := &cobra.Command{
		Use:   "inspect <pod>",
		Short: "Diagnose why a single pod crashed",
		Long: `inspect fetches the status, events, logs, and node conditions for a single
pod and prints a classified crash diagnosis.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(cmd, args, opts)
		},
	}

	fs := cmd.Flags()
	fs.StringVarP(&opts.container, "container", "c", "", "container to inspect (default: every container of the pod that warrants diagnosis)")
	fs.StringVar(&opts.output, "output", "human", "output format: human|json")
	fs.IntVar(&opts.previousLines, "previous-lines", 60, "number of lines to fetch from the previous container's log tail")
	fs.DurationVar(&opts.initStuckThresh, "init-stuck-threshold", 10*time.Minute, "how long an init container may run before it is considered stuck")
	// No -v shorthand: it is cobra's conventional shorthand for --version.
	fs.BoolVar(&opts.verbose, "verbose", false, "report every rule that matched, not just the primary diagnosis")

	opts.ai.addAIFlags(fs)

	// Namespace, kubeconfig, and context flags (-n/--namespace, --kubeconfig,
	// --context, etc.) come from genericclioptions.ConfigFlags below — do not
	// define a competing -n/--namespace flag here.
	opts.configFlags.AddFlags(fs)

	return cmd
}

// runInspect builds a cluster client from the standard kubeconfig flags and
// hands off to the inspect package, which owns every rendering and exit-code
// decision. Errors returned from here are pre-flight failures (bad kubeconfig,
// bad AI configuration) and are printed by Execute; anything inspect.Run
// reports itself comes back as an exitCodeError so it is not printed twice.
func runInspect(cmd *cobra.Command, args []string, opts *inspectOptions) error {
	restConfig, err := opts.configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}

	summarizer, err := opts.ai.buildSummarizer(cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	code := inspect.Run(cmd.Context(), client, inspect.Options{
		Namespace:          resolveNamespace(opts.configFlags),
		Pod:                args[0],
		Container:          opts.container,
		PreviousLines:      int64(opts.previousLines),
		InitStuckThreshold: opts.initStuckThresh,
		Output:             opts.output,
		Verbose:            opts.verbose,
		Summarizer:         summarizer,
		Server:             restConfig.Host,
		Out:                cmd.OutOrStdout(),
		ErrOut:             cmd.ErrOrStderr(),
	})
	return exitCode(code)
}

// resolveNamespace applies the documented precedence: the -n/--namespace flag
// wins, then the current kubeconfig context's namespace, then "default".
func resolveNamespace(flags *genericclioptions.ConfigFlags) string {
	if flags.Namespace != nil && *flags.Namespace != "" {
		return *flags.Namespace
	}
	if ns, _, err := flags.ToRawKubeConfigLoader().Namespace(); err == nil && ns != "" {
		return ns
	}
	return "default"
}
