package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ryckakas/crashcause/internal/watch"
)

// Client-side throttling for the controller's own API calls. client-go's
// defaults (5 QPS / 10 burst) are sized for a CLI, not for a controller that
// collects events and owner metadata during a cluster-wide crash storm; the
// pod-log rate limiter (--log-rate-limit) is the separate, stricter budget
// that protects the expensive endpoint.
const (
	watchClientQPS   = 20.0
	watchClientBurst = 30
)

type watchOptions struct {
	namespaces      []string
	selector        string
	metricsAddr     string
	lokiURL         string
	reemitInterval  time.Duration
	dedupTTL        time.Duration
	collectLogs     bool
	logNamespaces   []string
	logRateLimit    int
	previousLines   int
	leaderElect     bool
	leaderElectNS   string
	leaderElectID   string
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
	fs.StringVar(&opts.lokiURL, "loki-url", "", "Loki URL to send diagnoses to, either the base URL or the full /loki/api/v1/push URL (default: disabled)")
	fs.DurationVar(&opts.reemitInterval, "reemit-interval", time.Hour, "minimum interval before re-emitting an unchanged diagnosis for the same container")
	fs.DurationVar(&opts.dedupTTL, "dedup-ttl", 6*time.Hour, "how long a diagnosis is remembered for deduplication purposes")
	fs.BoolVar(&opts.collectLogs, "collect-logs", true, "fetch container logs as diagnosis evidence (requires the pods/log permission)")
	fs.StringSliceVar(&opts.logNamespaces, "log-namespaces", nil, "restrict log collection to these namespaces (default: all watched namespaces)")
	fs.IntVar(&opts.logRateLimit, "log-rate-limit", 10, "maximum pod log fetches per minute (0 or less disables the limit)")
	fs.IntVar(&opts.previousLines, "previous-lines", 60, "number of lines to fetch from the previous container's log tail")
	fs.BoolVar(&opts.leaderElect, "leader-elect", false, "enable leader election so only one replica of watch is active at a time")
	fs.StringVar(&opts.leaderElectNS, "leader-election-namespace", "", "namespace holding the leader election lease (default: $POD_NAMESPACE, else \"default\")")
	fs.StringVar(&opts.leaderElectID, "leader-election-id", "crashcause", "name of the leader election lease")
	fs.StringVar(&opts.healthAddr, "health-addr", ":8081", "address to serve health/readiness checks on")
	fs.DurationVar(&opts.initStuckThresh, "init-stuck-threshold", 10*time.Minute, "how long an init container may run before it is considered stuck")

	opts.ai.addAIFlags(fs)
	fs.StringSliceVar(&opts.aiNamespaces, "ai-namespaces", nil, "restrict AI summarization to these namespaces; empty keeps AI off everywhere, \"*\" enables it in all namespaces")

	// Kubeconfig and context flags (--kubeconfig, --context, etc.) come from
	// genericclioptions.ConfigFlags below. Per-pod namespace filtering for
	// watch is controlled by --namespaces above, not by ConfigFlags' -n.
	opts.configFlags.AddFlags(fs)

	return cmd
}

func runWatch(cmd *cobra.Command, _ []string, opts *watchOptions) error {
	restCfg, err := watchRESTConfig(cmd, opts.configFlags)
	if err != nil {
		return err
	}
	restCfg.QPS = watchClientQPS
	restCfg.Burst = watchClientBurst

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}

	summarizer, err := opts.ai.buildSummarizer(cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	controller, err := watch.New(client, watch.Config{
		Namespaces:              opts.namespaces,
		LabelSelector:           opts.selector,
		ReemitInterval:          opts.reemitInterval,
		DedupTTL:                opts.dedupTTL,
		CollectLogs:             opts.collectLogs,
		LogNamespaces:           opts.logNamespaces,
		LogRateLimit:            logRateLimit(opts.logRateLimit),
		PreviousLines:           int64(opts.previousLines),
		InitStuckThreshold:      opts.initStuckThresh,
		AINamespaces:            opts.aiNamespaces,
		MetricsAddr:             opts.metricsAddr,
		HealthAddr:              opts.healthAddr,
		LokiURL:                 opts.lokiURL,
		LeaderElect:             opts.leaderElect,
		LeaderElectionNamespace: opts.leaderElectNS,
		LeaderElectionID:        opts.leaderElectID,
	}, summarizer, cmd.OutOrStdout())
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancel the context; the controller then stops its
	// informers, flushes the sinks and shuts the HTTP servers down.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return controller.Run(ctx)
}

// logRateLimit converts the per-MINUTE flag value into the per-second rate
// the limiter works in. A non-positive value is an explicit operator opt-out
// and disables throttling entirely.
func logRateLimit(perMinute int) rate.Limit {
	if perMinute <= 0 {
		return rate.Inf
	}
	return rate.Limit(float64(perMinute) / 60.0)
}

// watchRESTConfig resolves the cluster connection the way a workload that can
// run both in and out of a cluster has to: the in-cluster service account
// first (the Helm-chart deployment case), falling back to the standard
// kubeconfig loading rules for local runs.
//
// An explicitly passed --kubeconfig or --context always wins, so that
// "point this at another cluster" keeps working even from inside a pod.
func watchRESTConfig(cmd *cobra.Command, flags *genericclioptions.ConfigFlags) (*rest.Config, error) {
	explicit := cmd.Flags().Changed("kubeconfig") || cmd.Flags().Changed("context")
	if !explicit {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	cfg, err := flags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return cfg, nil
}
