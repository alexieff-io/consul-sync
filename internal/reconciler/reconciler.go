package reconciler

import (
	"context"
	"log/slog"
	"time"

	"github.com/alexieff-io/consul-sync/internal/consul"
	"github.com/alexieff-io/consul-sync/internal/health"
	k8s "github.com/alexieff-io/consul-sync/internal/kubernetes"
	"github.com/alexieff-io/consul-sync/internal/metrics"
)

// Reconciler orchestrates the Consul watcher and Kubernetes syncer.
type Reconciler struct {
	watcher        *consul.Watcher
	syncer         *k8s.Syncer
	healthServer   *health.Server
	resyncInterval time.Duration
}

// New creates a new Reconciler.
func New(watcher *consul.Watcher, syncer *k8s.Syncer, healthServer *health.Server, resyncInterval time.Duration) *Reconciler {
	return &Reconciler{
		watcher:        watcher,
		syncer:         syncer,
		healthServer:   healthServer,
		resyncInterval: resyncInterval,
	}
}

// Run starts the reconciliation loop. It blocks until the context is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	watchCh, err := r.watcher.WatchServices(ctx)
	if err != nil {
		return err
	}

	resyncTicker := time.NewTicker(r.resyncInterval)
	defer resyncTicker.Stop()

	slog.Info("reconciler started", "resync_interval", r.resyncInterval)

	for {
		select {
		case <-ctx.Done():
			slog.Info("reconciler shutting down")
			return ctx.Err()

		case states, ok := <-watchCh:
			if !ok {
				slog.Info("watch channel closed")
				return nil
			}
			r.reconcile(ctx, states, "watch")

		case <-resyncTicker.C:
			slog.Info("performing scheduled resync")
			states, err := r.watcher.FetchAllServices(ctx)
			if err != nil {
				slog.Error("resync fetch failed", "error", err)
				metrics.ConsulErrors.Inc()
				metrics.ReconcileTotal.WithLabelValues("error").Inc()
				continue
			}
			r.reconcile(ctx, states, "resync")
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context, states []consul.ServiceState, trigger string) {
	slog.Info("reconciling", "trigger", trigger, "services", len(states))

	if err := r.syncer.Sync(ctx, states); err != nil {
		slog.Error("sync failed", "trigger", trigger, "error", err)
		metrics.ReconcileTotal.WithLabelValues("error").Inc()
		return
	}

	metrics.ReconcileTotal.WithLabelValues("success").Inc()
	r.healthServer.SetReady()
	slog.Info("reconciliation complete", "trigger", trigger, "services", len(states))
}
