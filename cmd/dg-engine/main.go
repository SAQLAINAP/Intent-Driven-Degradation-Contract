// Command dg-engine is the IDDC runtime engine.
//
// It loads a compiled .dg policy bundle, starts the signal bus and tier
// evaluator, and enforces tier transitions (sidecar flags, write queue,
// Kubernetes), holding gated transitions for an operator decision.
//
// Usage:
//
//	dg-engine --policy policy.dg [--config config/dg-engine.yaml]
//
// Servers (addresses configurable under server: in dg-engine.yaml):
//
//	:9191  control API + web console   (GET / opens the console)
//	:8081  sidecar                     (GET /flags, /tier, /health)
//	:9292  gate callbacks              (POST /gate/{approve,override,escalate})
//	:9090  Prometheus metrics          (GET /metrics)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/internal/version"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/audit"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/collector"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/compiler"
	appconfig "github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/config"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/enforcer"
	k8sctl "github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/k8s"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/sidecar"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/writequeue"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/gate"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/runtime"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/sigfetch"
)

func main() {
	var policyPath string
	var configPath string

	root := &cobra.Command{
		Use:     "dg-engine",
		Short:   "IDDC runtime engine — enforces degradation contracts continuously",
		Version: version.String(),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return run(policyPath, configPath)
		},
	}

	root.Flags().StringVar(&policyPath, "policy", "", "Path to compiled .dg bundle (required)")
	root.Flags().StringVar(&configPath, "config", "config/dg-engine.yaml", "Path to dg-engine.yaml config")
	_ = root.MarkFlagRequired("policy") // only fails if the flag is undefined

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(policyPath, configPath string) error {
	// ── Load config ───────────────────────────────────────────────────────────
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// ── Load compiled policy bundle ───────────────────────────────────────────
	bundle, err := compiler.ReadBundleFromFile(policyPath)
	if err != nil {
		return fmt.Errorf("loading policy bundle %s: %w", policyPath, err)
	}
	if len(bundle.Tiers) == 0 {
		return fmt.Errorf("policy bundle %s declares no tiers", policyPath)
	}
	printBanner(bundle)

	// ── Audit logger ──────────────────────────────────────────────────────────
	auditLog, err := newAuditLogger(cfg.Audit)
	if err != nil {
		return fmt.Errorf("creating audit logger: %w", err)
	}

	// ── Signal sources ────────────────────────────────────────────────────────
	store := sigfetch.NewInjectedStore()
	fetchers, injectable := buildFetchers(bundle, cfg, store)

	// ── Enforcement components ────────────────────────────────────────────────
	humanGate := gate.NewHumanGate(gate.Config{
		SlackWebhookURL:     cfg.Gate.SlackWebhookURL,
		CallbackBaseURL:     cfg.Gate.CallbackBaseURL,
		AuthToken:           cfg.Gate.AuthToken,
		PagerDutyRoutingKey: cfg.Gate.PagerDutyRoutingKey,
	}, auditLog)
	gateSrv := gate.NewCallbackServer(cfg.Server.GateAddr, humanGate)
	sidecarSrv := sidecar.NewServer(cfg.Server.SidecarAddr, cfg.Gate.AuthToken)
	writeQueue := buildWriteQueue(bundle)

	var reconciler enforcer.Reconciler
	if cfg.K8s.DeploymentName != "" {
		rec, err := k8sctl.NewReconciler(k8sctl.Config{
			KubeconfigPath: cfg.K8s.KubeconfigPath,
			Namespace:      cfg.K8s.Namespace,
			DeploymentName: cfg.K8s.DeploymentName,
			TargetWarm:     cfg.K8s.TargetWarm,
			MaxPods:        cfg.K8s.MaxPods,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[k8s]     reconciler init failed (k8s enforcement disabled): %v\n", err)
		} else {
			reconciler = rec
			fmt.Printf("[k8s]     reconciler ready  namespace=%s  deployment=%s\n",
				cfg.K8s.Namespace, cfg.K8s.DeploymentName)
		}
	}

	// ── Runtime engine ────────────────────────────────────────────────────────
	evalCfg := runtime.EvaluatorConfig{
		UpHysteresis:   cfg.Evaluator.UpHysteresis,
		DownHysteresis: cfg.Evaluator.DownHysteresis,
		TickInterval:   cfg.SignalBus.TickInterval,
	}
	var cadences map[string]time.Duration
	if cfg.SignalBus.Mode == "inject" {
		// Injected values live in memory, so poll them every tick for snappy demos.
		cadences = make(map[string]time.Duration, len(bundle.SignalSources))
		for _, s := range bundle.SignalSources {
			cadences[s.ID] = cfg.SignalBus.TickInterval
		}
	}
	engine := runtime.NewEngine(bundle, fetchers, cadences, evalCfg)
	engine.Bus().SetStaleTimeout(cfg.SignalBus.StaleSignalTimeout)

	enf := enforcer.New(enforcer.Config{
		Bundle:     bundle,
		Sidecar:    sidecarSrv,
		Queue:      writeQueue,
		Reconciler: reconciler,
		Gate:       humanGate,
		Locker:     engine,
		Audit:      auditLog,
		Out:        os.Stdout,
		OnGateOpen: func(from, to string, spec *policy.GateSpec) {
			printGateInstructions(cfg, to)
		},
	})
	engine.RegisterHandler(enf.HandleTransition)

	// ── Context — cancelled by SIGINT/SIGTERM ─────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── Servers ───────────────────────────────────────────────────────────────
	startServer("[gate]    callbacks listening on "+cfg.Server.GateAddr, gateSrv.Start, ctx)
	startServer("[sidecar] listening on "+cfg.Server.SidecarAddr, sidecarSrv.Start, ctx)

	control := &controlAPI{
		bundle:     bundle,
		engine:     engine,
		enforcer:   enf,
		store:      store,
		injectable: injectable,
		queue:      writeQueue,
		sidecar:    sidecarSrv,
		gate:       humanGate,
		gateRoutes: gateSrv.Handler(),
		audit:      auditLog,
	}
	go serveHTTP(&http.Server{
		Addr:              cfg.Server.ControlAddr,
		Handler:           control.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}, ctx)
	fmt.Printf("[control] listening on %s  — web console: http://localhost%s/\n",
		cfg.Server.ControlAddr, cfg.Server.ControlAddr)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	go serveHTTP(&http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Metrics.PrometheusPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}, ctx)
	fmt.Printf("[metrics] listening on :%d/metrics\n", cfg.Metrics.PrometheusPort)

	// ── Start engine (blocks until ctx is cancelled) ──────────────────────────
	fmt.Printf("\n[engine]  started  service=%s  tiers=%d  signal_mode=%s\n",
		bundle.Service, len(bundle.Tiers), cfg.SignalBus.Mode)
	fmt.Printf("[engine]  current tier: %s\n\n", engine.CurrentTier())
	engine.Start(ctx)

	fmt.Println("\n[engine]  shut down cleanly")
	return nil
}

// buildFetchers picks a fetcher for every declared signal and reports which
// signals are driven by the injected store (and therefore accept POST /inject).
func buildFetchers(bundle *compiler.CompiledBundle, cfg *appconfig.Config, store *sigfetch.InjectedStore) (map[string]runtime.SignalFetcher, map[string]bool) {
	seed := make(map[string]float64, len(bundle.SignalSources))
	for _, s := range bundle.SignalSources {
		seed[s.ID] = 0
	}
	store.Set(seed)

	proc := collector.NewProcCollector()
	fetchers := make(map[string]runtime.SignalFetcher, len(bundle.SignalSources))
	injectable := make(map[string]bool, len(bundle.SignalSources))
	for _, s := range bundle.SignalSources {
		id := s.ID
		source := "injected"
		switch {
		case cfg.SignalBus.Mode == "inject":
		case s.Source == "http" && s.URL != "":
			fetchers[id] = runtime.SignalFetcher(sigfetch.NewHTTPFetcher(s.URL, s.Field))
			source = "http " + s.URL
		case s.Source == "prometheus" && cfg.Metrics.PrometheusURL != "":
			query := s.Query
			if query == "" {
				query = prometheusQueryForSignal(id)
			}
			fetchers[id] = runtime.SignalFetcher(sigfetch.NewPrometheusFetcher(cfg.Metrics.PrometheusURL, query))
			source = "prometheus " + query
		case s.Source == "proc" && goruntime.GOOS == "linux":
			fetchers[id] = func(ctx context.Context) (float64, error) { return proc.Collect(ctx, id) }
			source = "proc"
		}
		if fetchers[id] == nil {
			fetchers[id] = runtime.SignalFetcher(store.FetcherFor(id))
			injectable[id] = true
		}
		fmt.Printf("[signal]  %-16s ← %s\n", id, source)
	}
	fmt.Println()
	return fetchers, injectable
}

// ─── server helpers ───────────────────────────────────────────────────────────

type contextStarter func(ctx context.Context) error

// startServer launches a contextStarter (e.g. gate.CallbackServer.Start) in a goroutine.
func startServer(banner string, fn contextStarter, ctx context.Context) {
	fmt.Println(banner)
	go func() {
		if err := fn(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "%s — error: %v\n", banner, err)
		}
	}()
}

// serveHTTP starts an http.Server and shuts it down gracefully when ctx is cancelled.
func serveHTTP(srv *http.Server, ctx context.Context) {
	go func() {
		<-ctx.Done()
		// ctx is already cancelled here; keep its values but give shutdown a fresh deadline.
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx) // best effort: the process is exiting
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server %s error: %v\n", srv.Addr, err)
	}
}

// ─── display helpers ──────────────────────────────────────────────────────────

func printBanner(b *compiler.CompiledBundle) {
	fmt.Printf("╔══════════════════════════════════════════════════╗\n")
	fmt.Printf("║  IDDC — Intent-Driven Degradation Contracts      ║\n")
	fmt.Printf("╚══════════════════════════════════════════════════╝\n")
	fmt.Printf("  service : %s\n", b.Service)
	fmt.Printf("  policy  : v%s\n", b.Version)
	fmt.Printf("  engine  : %s\n", version.Get())
	fmt.Printf("  tiers   : %d\n", len(b.Tiers))
	fmt.Printf("  signals : %d\n\n", len(b.SignalSources))
	fmt.Println("  Tier ladder (most degraded matching tier wins):")
	for i, t := range b.Tiers {
		gateStr := ""
		if t.GateSpec != nil {
			gateStr = fmt.Sprintf("  ← gate (wait=%s  on_timeout=%s)", t.GateSpec.Wait, t.GateSpec.OnTimeout)
		}
		fmt.Printf("    %d. %-12s  mode=%-15s%s\n", i, t.Name, t.Behavior.Mode, gateStr)
	}
	fmt.Println()
}

// printGateInstructions tells the operator what to do when no Slack URL is configured.
func printGateInstructions(cfg *appconfig.Config, tier string) {
	if cfg.Gate.SlackWebhookURL != "" {
		return // Slack will handle notification
	}
	base := cfg.Gate.CallbackBaseURL
	if base == "" {
		base = "http://localhost" + cfg.Server.GateAddr
	}
	fmt.Printf("\n╔══════════════════════════════════════════════════╗\n")
	fmt.Printf("║  GATE OPEN — tier: %-30s║\n", tier)
	fmt.Printf("╠══════════════════════════════════════════════════╣\n")
	fmt.Printf("║  Console :  http://localhost%s/\n", cfg.Server.ControlAddr)
	fmt.Printf("║  Approve :  POST %s/gate/approve\n", base)
	fmt.Printf("║  Override:  POST %s/gate/override\n", base)
	fmt.Printf("║             body: {\"target_tier\":\"nominal\",\"duration_min\":10}\n")
	fmt.Printf("║  Escalate:  POST %s/gate/escalate\n", base)
	fmt.Printf("║             body: {\"message\":\"paging SRE\"}\n")
	fmt.Printf("╚══════════════════════════════════════════════════╝\n\n")
}

// ─── config helpers ───────────────────────────────────────────────────────────

// prometheusQueryForSignal returns a default PromQL expression for known signal IDs.
// Declare query: on the signal to override it.
func prometheusQueryForSignal(signalID string) string {
	defaults := map[string]string{
		"rps":            `rate(http_requests_total[1m])`,
		"db_latency_p99": `histogram_quantile(0.99, rate(db_query_duration_seconds_bucket[5m])) * 1000`,
		"pod_ceiling":    `kube_deployment_status_replicas / kube_deployment_spec_replicas_max`,
		"s3_error_rate":  `rate(s3_requests_total{status="error"}[5m])`,
		"mem_pressure":   `1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`,
	}
	if q, ok := defaults[signalID]; ok {
		return q
	}
	return signalID
}

// loadConfig loads dg-engine.yaml. If the file is absent, silently uses defaults
// (fine for local dev without a config file).
func loadConfig(path string) (*appconfig.Config, error) {
	cfg, err := appconfig.LoadFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			d := appconfig.Defaults()
			return &d, nil
		}
		return nil, err
	}
	return cfg, nil
}

func newAuditLogger(cfg appconfig.AuditConfig) (*audit.Logger, error) {
	if cfg.Output == "" || cfg.Output == "stdout" {
		return audit.NewStdoutLogger(), nil
	}
	return audit.NewFileLogger(cfg.Output)
}

// buildWriteQueue scans the bundle for the first tier with queue_writes configured
// and returns a shared Queue, or nil if no tier uses it.
func buildWriteQueue(bundle *compiler.CompiledBundle) *writequeue.Queue {
	for _, t := range bundle.Tiers {
		if t.Behavior.QueueWrites != nil {
			qs := t.Behavior.QueueWrites
			maxDepth := qs.MaxDepth
			if maxDepth <= 0 {
				maxDepth = 1000
			}
			ttl := qs.TTL
			if ttl == 0 {
				ttl = 60 * time.Second
			}
			return writequeue.NewQueue(bundle.Service, maxDepth, ttl)
		}
	}
	return nil
}
