package watch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Leader-election timings. These are the client-go defaults used by most
// controllers: a 15s lease renewed every 2s with a 10s renew deadline, which
// tolerates brief API-server hiccups without either flapping or leaving the
// cluster unwatched for long.
const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second

	// leaderWorkDrainTimeout bounds how long Run waits for the work loop to
	// unwind after leadership was lost, so a wedged shutdown cannot hang the
	// process instead of letting the new leader take over.
	leaderWorkDrainTimeout = shutdownTimeout
)

// runLeaderElected runs the work loop only while this replica holds the
// lease. Losing the lease is a clean stop: runWork's context is canceled,
// the informers stop, and Run returns errLostLeadership so the supervisor
// restarts the process rather than leaving a half-stopped controller behind.
func (c *Controller) runLeaderElected(ctx context.Context) error {
	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		c.cfg.LeaderElectionNamespace,
		c.cfg.LeaderElectionID,
		c.client.CoreV1(),
		c.client.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: leaderIdentity()},
	)
	if err != nil {
		return fmt.Errorf("watch: build leader election lock: %w", err)
	}

	// Buffered so the work goroutine can always report and exit, even if
	// nobody is left waiting for it.
	workErr := make(chan error, 1)

	// A standby replica never runs OnStartedLeading, so nothing will ever
	// send on workErr; waiting for it would stall shutdown for the full
	// drain timeout and log a misleading warning.
	var led atomic.Bool

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Name:            c.cfg.LeaderElectionID,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				led.Store(true)
				workErr <- c.runWork(leaderCtx)
			},
			OnStoppedLeading: func() {
				slog.Info("crashcause watch stopped leading")
			},
		},
	})
	if err != nil {
		return fmt.Errorf("watch: configure leader election: %w", err)
	}

	slog.Info("crashcause watch waiting for leader election",
		"namespace", c.cfg.LeaderElectionNamespace, "lease", c.cfg.LeaderElectionID)
	elector.Run(ctx)

	// Run returns as soon as the lease is lost, but OnStartedLeading runs in
	// its own goroutine; wait (briefly) for it so shutdown is ordered. A
	// replica that never led has no work loop to wait for.
	if led.Load() {
		select {
		case err := <-workErr:
			if err != nil {
				return err
			}
		case <-time.After(leaderWorkDrainTimeout):
			slog.Warn("crashcause watch work loop did not stop within the drain timeout")
		}
	}

	if ctx.Err() == nil {
		return errLostLeadership
	}
	return nil
}

// leaderIdentity is the lease holder identity: the pod's hostname plus a
// per-process suffix, so a restarted pod with the same name never looks like
// the previous incarnation still holding the lease.
func leaderIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "crashcause"
	}
	return host + "_" + string(uuid.NewUUID())
}
