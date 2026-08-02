package watch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// httpReadHeaderTimeout protects the tiny health/metrics servers from
// slowloris-style clients; they serve trivial responses and never need long.
const httpReadHeaderTimeout = 5 * time.Second

// startServers binds the health and metrics listeners and starts serving.
//
// Both endpoints live outside the leader-election gate on purpose: a standby
// replica must still answer /healthz (it is alive and correctly waiting) and
// still expose whatever it has counted, otherwise Kubernetes would restart
// every non-leader.
//
// /readyz flips to 200 only once the informer caches are synced, so a rolling
// upgrade cannot cut over to a replica that has not yet seen the cluster.
func (c *Controller) startServers(ctx context.Context) ([]*http.Server, error) {
	health := strings.TrimSpace(c.cfg.HealthAddr)
	metrics := strings.TrimSpace(c.cfg.MetricsAddr)

	// Identical addresses are a legitimate configuration (one port, all three
	// paths); binding twice would just fail with "address already in use".
	if metrics != "" && metrics == health {
		mux := http.NewServeMux()
		c.registerHealthRoutes(mux)
		mux.Handle("/metrics", c.prom.Handler())
		srv, addr, err := serveMux(ctx, metrics, mux)
		if err != nil {
			return nil, err
		}
		c.setBoundAddrs(addr, addr)
		return []*http.Server{srv}, nil
	}

	var servers []*http.Server
	var healthBound, metricsBound string

	if health != "" {
		mux := http.NewServeMux()
		c.registerHealthRoutes(mux)
		srv, addr, err := serveMux(ctx, health, mux)
		if err != nil {
			return nil, err
		}
		servers = append(servers, srv)
		healthBound = addr
	}

	if metrics != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", c.prom.Handler())
		srv, addr, err := serveMux(ctx, metrics, mux)
		if err != nil {
			shutdownServers(ctx, servers)
			return nil, err
		}
		servers = append(servers, srv)
		metricsBound = addr
	}

	c.setBoundAddrs(healthBound, metricsBound)
	return servers, nil
}

// registerHealthRoutes wires /healthz (liveness: always 200 once Run has
// started the server) and /readyz (readiness: 200 only after cache sync).
func (c *Controller) registerHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !c.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("informer caches not synced\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}

// serveMux binds addr and serves mux in the background, returning the server
// and the ADDRESS ACTUALLY BOUND (which differs from addr when the caller
// asked for port 0, as tests do).
//
// ctx only bounds the listen call itself: (*net.ListenConfig).Listen uses it
// to give up on a slow bind, but the returned listener's lifetime is
// independent of ctx, so it keeps serving after ctx is done (shutdownServers,
// not ctx cancellation, is what stops it).
func serveMux(ctx context.Context, addr string, mux *http.ServeMux) (*http.Server, string, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("watch: listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !isServerClosed(err) {
			slog.Error("crashcause http server stopped", "addr", ln.Addr().String(), "error", err)
		}
	}()
	return srv, ln.Addr().String(), nil
}

func isServerClosed(err error) bool {
	return errors.Is(err, http.ErrServerClosed)
}

// shutdownServers gracefully stops every server under one bounded deadline.
//
// ctx is derived from the caller's context so contextcheck sees the causal
// link, but with context.WithoutCancel: callers (notably Controller.Run's
// deferred shutdown) invoke this exactly when their ctx has already been
// canceled, and shutting down with an already-dead context would abort the
// graceful drain immediately instead of honoring shutdownTimeout. Context
// values (trace IDs, etc.) still propagate; the caller's cancellation signal
// and deadline are dropped in favor of shutdownTimeout.
func shutdownServers(ctx context.Context, servers []*http.Server) {
	if len(servers) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("crashcause http server shutdown", "error", err)
		}
	}
}

// setBoundAddrs publishes the addresses the listeners actually bound to.
func (c *Controller) setBoundAddrs(health, metrics string) {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()
	c.boundHealthAddr = health
	c.boundMetricsAddr = metrics
}

// healthAddr and metricsAddr report the bound addresses (empty when the
// endpoint is disabled). They exist for tests that bind port 0.
func (c *Controller) healthAddr() string {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()
	return c.boundHealthAddr
}

func (c *Controller) metricsAddr() string {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()
	return c.boundMetricsAddr
}
