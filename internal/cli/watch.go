package cli

import (
	"errors"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// watchOptions holds all flag-bound settings for the watch command.
type watchOptions struct {
	namespaces      []string
	selector        string
	metricsAddr     string
	lokiURL         string
	reemitInterval  time.Duration
	dedupTTL        time.Duration
	logRateLimit    int
	leaderElect     bool
	healthAddr      string
	ai              aiOptions
	aiNamespaces    []string
	initStuckThresh time.Duration
	configFlags     *genericclioptions.ConfigFlags
}

func newWatchCmd() *cobra.Command {
	opts := &watchOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
	}

	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Continuously watch a cluster and emit crash diagnoses",
		Long: `watch observes pods across a cluster, classifies crashes as they occur, and
emits diagnoses to one or more sinks (stdout, Prometheus, Loki).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(cmd, args, opts)
		},
	}

	fs := cmd.Flags()
	fs.StringSliceVar(&opts.namespaces, "namespaces", nil, "namespaces to watch (default: all namespaces)")
	fs.StringVar(&opts.selector, "selector", "", "label selector to filter watched pods")
	fs.StringVar(&opts.metricsAddr, "metrics-addr", "", "address to serve Prometheus metrics on, e.g. :9090 (default: disabled)")
	fs.StringVar(&opts.lokiURL, "loki-url", "", "Loki push URL to send diagnoses to (default: disabled)")
	fs.DurationVar(&opts.reemitInterval, "reemit-interval", time.Hour, "minimum interval before re-emitting an unchanged diagnosis for the same container")
	fs.DurationVar(&opts.dedupTTL, "dedup-ttl", 6*time.Hour, "how long a diagnosis is remembered for deduplication purposes")
	fs.IntVar(&opts.logRateLimit, "log-rate-limit", 10, "maximum pod log fetches per minute")
	fs.BoolVar(&opts.leaderElect, "leader-elect", false, "enable leader election so only one replica of watch is active at a time")
	fs.StringVar(&opts.healthAddr, "health-addr", ":8081", "address to serve health/readiness checks on")
	fs.DurationVar(&opts.initStuckThresh, "init-stuck-threshold", 10*time.Minute, "how long an init container may run before it is considered stuck")

	opts.ai.addAIFlags(fs)
	fs.StringSliceVar(&opts.aiNamespaces, "ai-namespaces", nil, "restrict AI summarization to these namespaces (default: all namespaces where --ai is enabled)")

	// Kubeconfig and context flags (--kubeconfig, --context, etc.) come from
	// genericclioptions.ConfigFlags below. Per-pod namespace filtering for
	// watch is controlled by --namespaces above, not by ConfigFlags' -n.
	opts.configFlags.AddFlags(fs)

	return cmd
}

func runWatch(_ *cobra.Command, _ []string, _ *watchOptions) error {
	return errors.New("watch: not yet implemented")
}
