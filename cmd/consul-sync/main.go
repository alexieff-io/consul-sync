package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/alexieff-io/consul-sync/internal/consul"
	"github.com/alexieff-io/consul-sync/internal/health"
	k8s "github.com/alexieff-io/consul-sync/internal/kubernetes"
	"github.com/alexieff-io/consul-sync/internal/reconciler"
)

// Set via -ldflags at build time.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("consul-sync %s (commit: %s)\n", version, commit)
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := loadConfig()
	slog.Info("starting consul-sync",
		"version", version,
		"commit", commit,
		"consul_addr", cfg.consulAddr,
		"consul_tag", cfg.consulTag,
		"target_namespace", cfg.targetNamespace,
		"metrics_addr", cfg.metricsAddr,
		"resync_interval", cfg.resyncInterval,
		"enable_httproutes", cfg.routeCfg.Enabled,
		"domain_suffix", cfg.routeCfg.DomainSuffix,
		"internal_gateway", cfg.routeCfg.InternalGateway,
		"external_gateway", cfg.routeCfg.ExternalGateway,
		"gateway_namespace", cfg.routeCfg.GatewayNamespace,
		"gateway_listener", cfg.routeCfg.GatewayListener,
		"internal_tag", cfg.routeCfg.InternalTag,
		"external_tag", cfg.routeCfg.ExternalTag,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Kubernetes client
	k8sClient, dynClient, err := newKubernetesClients()
	if err != nil {
		slog.Error("failed to create kubernetes client", "error", err)
		os.Exit(1)
	}

	// Components
	watcher := consul.NewWatcher(cfg.consulAddr, cfg.consulToken, cfg.consulTag)
	syncer := k8s.NewSyncer(k8sClient, dynClient, cfg.targetNamespace, cfg.routeCfg)
	healthSrv := health.NewServer(cfg.metricsAddr, version, commit)
	rec := reconciler.New(watcher, syncer, healthSrv, cfg.resyncInterval)

	// Start health/metrics server
	go func() {
		if err := healthSrv.ListenAndServe(); err != nil {
			slog.Error("health server error", "error", err)
			cancel()
		}
	}()

	// Run reconciler (blocks until context cancelled)
	if err := rec.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("reconciler failed", "error", err)
		os.Exit(1)
	}

	// Gracefully shut down the health server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("health server shutdown error", "error", err)
	}

	slog.Info("consul-sync stopped")
}

type config struct {
	consulAddr      string
	consulToken     string
	consulTag       string
	targetNamespace string
	metricsAddr     string
	resyncInterval  time.Duration
	routeCfg        k8s.HTTPRouteConfig
}

func loadConfig() config {
	targetNamespace := envOrDefault("TARGET_NAMESPACE", "network")

	cfg := config{
		consulAddr:      os.Getenv("CONSUL_ADDR"),
		consulToken:     os.Getenv("CONSUL_TOKEN"),
		consulTag:       envOrDefault("CONSUL_TAG", "kubernetes"),
		targetNamespace: targetNamespace,
		metricsAddr:     envOrDefault("METRICS_ADDR", ":8080"),
		routeCfg: k8s.HTTPRouteConfig{
			Enabled:          strings.ToLower(envOrDefault("ENABLE_HTTPROUTES", "true")) == "true",
			DomainSuffix:     envOrDefault("DOMAIN_SUFFIX", "k8s.alexieff.io"),
			InternalGateway:  envOrDefault("INTERNAL_GATEWAY", "envoy-internal"),
			ExternalGateway:  envOrDefault("EXTERNAL_GATEWAY", "envoy-external"),
			GatewayNamespace: envOrDefault("GATEWAY_NAMESPACE", targetNamespace),
			GatewayListener:  envOrDefault("GATEWAY_LISTENER", "https"),
			InternalTag:      envOrDefault("INTERNAL_TAG", "internal"),
			ExternalTag:      envOrDefault("EXTERNAL_TAG", "external"),
		},
	}

	if cfg.consulAddr == "" {
		fmt.Fprintln(os.Stderr, "CONSUL_ADDR is required")
		os.Exit(1)
	}

	resyncStr := envOrDefault("RESYNC_INTERVAL", "5m")
	var err error
	cfg.resyncInterval, err = time.ParseDuration(resyncStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid RESYNC_INTERVAL %q: %v\n", resyncStr, err)
		os.Exit(1)
	}

	return cfg
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func newKubernetesClients() (kubernetes.Interface, dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fallback to kubeconfig for local development
		kubeconfigPath := os.Getenv("KUBECONFIG")
		if kubeconfigPath == "" {
			kubeconfigPath = os.Getenv("HOME") + "/.kube/config"
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, nil, fmt.Errorf("building kubeconfig: %w", err)
		}
	}

	k8sClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return k8sClient, dynClient, nil
}
