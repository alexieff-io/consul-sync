package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	SyncedServices = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "consul_sync_services_total",
		Help: "Number of currently synced services",
	})

	SyncedEndpoints = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "consul_sync_endpoints_total",
		Help: "Total number of endpoints across all synced services",
	})

	ReconcileTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "consul_sync_reconcile_total",
		Help: "Total reconciliations performed",
	}, []string{"status"})

	ConsulErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "consul_sync_consul_errors_total",
		Help: "Total errors communicating with Consul",
	})

	KubernetesErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "consul_sync_kubernetes_errors_total",
		Help: "Total errors communicating with the Kubernetes API",
	})
)
