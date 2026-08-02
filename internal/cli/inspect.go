package cli

import (
	"errors"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// inspectOptions holds all flag-bound settings for the inspect command.
type inspectOptions struct {
	container       string
	output          string
	previousLines   int
	initStuckThresh time.Duration
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
	fs.StringVarP(&opts.container, "container", "c", "", "container to inspect (default: the only container, or the first if unambiguous)")
	fs.StringVar(&opts.output, "output", "human", "output format: human|json")
	fs.IntVar(&opts.previousLines, "previous-lines", 60, "number of lines to fetch from the previous container's log tail")
	fs.DurationVar(&opts.initStuckThresh, "init-stuck-threshold", 10*time.Minute, "how long an init container may run before it is considered stuck")

	opts.ai.addAIFlags(fs)

	// Namespace, kubeconfig, and context flags (-n/--namespace, --kubeconfig,
	// --context, etc.) come from genericclioptions.ConfigFlags below — do not
	// define a competing -n/--namespace flag here.
	opts.configFlags.AddFlags(fs)

	return cmd
}

func runInspect(_ *cobra.Command, _ []string, _ *inspectOptions) error {
	return errors.New("inspect: not yet implemented")
}
