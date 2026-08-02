// Package cli implements the crashcause command-line interface: the root
// command plus the inspect and watch subcommands. This package wires up
// flags and logging only — the actual pod inspection and cluster-watch
// logic is implemented in later milestones.
package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// BuildInfo carries version metadata injected at link time via
// -X main.version=..., -X main.commit=..., -X main.date=... (see
// cmd/crashcause/main.go). It is passed into NewRootCmd so this package
// stays independent of the main package's package-level variables.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// NewRootCmd constructs the root "crashcause" command.
func NewRootCmd(build BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:   "crashcause",
		Short: "Explain why a Kubernetes pod crashed",
		Long: `crashcause answers "why did this pod crash?"

It pulls pod status, container states, events, logs, and node conditions
directly from the Kubernetes API and classifies the crash into a stable,
machine-readable cause code (e.g. oom_killed, image_pull_auth,
probe_liveness_failure) along with a human-readable explanation and
suggested next steps.`,
		Version:           fmt.Sprintf("%s (commit %s, built %s)", build.Version, build.Commit, build.Date),
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: persistentPreRunE,
	}

	root.PersistentFlags().String("log-level", "info", "log verbosity: debug|info|warn|error")

	root.AddCommand(newInspectCmd())
	root.AddCommand(newWatchCmd())

	return root
}

// persistentPreRunE parses --log-level and installs the default slog
// handler before any subcommand runs.
func persistentPreRunE(cmd *cobra.Command, _ []string) error {
	levelStr, err := cmd.Flags().GetString("log-level")
	if err != nil {
		return err
	}

	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("invalid --log-level %q: must be one of debug|info|warn|error", levelStr)
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	return nil
}

// Execute builds and runs the root command, returning the process exit
// code. Errors are printed to stderr; usage is not dumped on error since
// most errors at this stage are "not yet implemented", not misuse.
func Execute(build BuildInfo) int {
	root := NewRootCmd(build)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
