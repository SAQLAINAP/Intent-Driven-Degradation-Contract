// Command dg-engine is the IDDC runtime engine.
//
// It loads a compiled .dg policy bundle, starts the signal bus and tier
// evaluator, and enforces tier transitions by calling registered handlers.
//
// Usage:
//
//	dg-engine --policy config/example-degradation.dg [--config config/dg-engine.yaml]
//
// Endpoints:
//
//	POST http://localhost:9191/inject          body: {"rps": 2500}          — inject signal values
//	GET  http://localhost:9191/signals                                       — view current signal values
//	POST http://localhost:9191/write           body: {"id":"w1","payload":.} — buffer a write (when queue active)
//	GET  http://localhost:9191/write/depth                                   — queue depth + drop count
//	POST http://localhost:9191/override        body: {"tier":"nominal","duration_min":10} — manual tier lock
//	GET  http://localhost:9090/metrics                                       — Prometheus metrics
//	POST http://localhost:9292/gate/{approve,override,escalate}             — gate callbacks
//	GET  http://localhost:8081/flags                                         — sidecar feature flags
//	GET  http://localhost:8081/tier                                          — current tier state
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"iddc/pkg/audit"
	"iddc/pkg/compiler"
	appconfig "iddc/pkg/config"
	k8sctl "iddc/pkg/enforcement/k8s"
	"iddc/pkg/enforcement/sidecar"
	"iddc/pkg/enforcement/writequeue"
	"iddc/pkg/gate"
	"iddc/pkg/policy"
	"iddc/pkg/runtime"
	"iddc/pkg/sigfetch"
)

// gateTimeout is added on top of the gate spec's wait time so the context
// isn't cancelled before the gate can record its outcome.
const gateTimeout = 10 * time.Second

func main() {
	var policyPath string
	var configPath string

	root := &cobra.Command{
		Use:   "dg-engine",
		Short: "IDDC runtime engine — enforces degradation contracts continuously",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(policyPath, configPath)
		},
	}

	root.Flags().StringVar(&policyPath, "policy", "", "Path to compiled .dg bundle (required)")
	root.Flags().StringVar(&configPath, "config", "config/dg-engine.yaml", "Path to dg-engine.yaml config")
	root.MarkFlagRequired("policy")

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
	printBanner(bundle)

	// ── Audit logger ──────────────────────────────────────────────────────────
	auditLog, err := newAuditLogger(cfg.Audit)
	if err != nil {
		return fmt.Errorf("creating audit logger: %w", err)
	}

	// ── Signal store (injected values for local dev) ──────────────────────────
	// In production replace with sigfetch.NewPrometheusFetcher() per signal.
	store := sigfetch.NewInjectedStore()

	// Seed all declared signals to 0 so the bus starts clean.
	seed := make(map[string]float64, len(bundle.SignalSources))
	for _, s := range bundle.SignalSources {
		seed[s.ID] = 0
	}
	store.Set(seed)

	// Build fetcher map — one per declared signal.
	fetchers := make(map[string]runtime.SignalFetcher, len(bundle.SignalSources))
	for _, s := range bundle.SignalSources {
		id := s.ID // capture for closure
		switch {
		case s.Source == "http" && s.URL != "":
			fetchers[id] = runtime.SignalFetcher(sigfetch.NewHTTPFetcher(s.URL, s.Field))
		case cfg.Metrics.PrometheusURL != "":
			fetchers[id] = runtime.SignalFetcher(
				sigfetch.NewPrometheusFetcher(cfg.Metrics.PrometheusURL, prometheusQueryForSignal(id)),
			)
		default:
			fetchers[id] = runtime.SignalFetcher(store.FetcherFor(id))
		}
	}

	// ── Human gate ────────────────────────────────────────────────────────────
	humanGate := gate.NewHumanGate(gate.Config{
		SlackWebhookURL:     cfg.Gate.SlackWebhookURL,
		CallbackBaseURL:     cfg.Gate.CallbackBaseURL,
		AuthToken:           cfg.Gate.AuthToken,
		PagerDutyRoutingKey: cfg.Gate.PagerDutyRoutingKey,
	}, auditLog)

	// ── Sidecar server ────────────────────────────────────────────────────────
	sidecarSrv := sidecar.NewServer(":8081", cfg.Gate.AuthToken)

	// ── Write queue ───────────────────────────────────────────────────────────
	// Create a shared queue if any tier declares queue_writes.
	writeQueue := buildWriteQueue(bundle)

	// ── Kubernetes reconciler (optional — enabled when deployment_name is set) ──
	var k8sReconciler *k8sctl.Reconciler
	if cfg.K8s.DeploymentName != "" {
		rec, err := k8sctl.NewReconciler(k8sctl.Config{
			KubeconfigPath: cfg.K8s.KubeconfigPath,
			Namespace:      cfg.K8s.Namespace,
			DeploymentName: cfg.K8s.DeploymentName,
			TargetWarm:     cfg.K8s.TargetWarm,
			MaxPods:        cfg.K8s.MaxPods,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[k8s] reconciler init failed (k8s enforcement disabled): %v\n", err)
		} else {
			k8sReconciler = rec
			fmt.Printf("[k8s]    reconciler ready  namespace=%s  deployment=%s\n",
				cfg.K8s.Namespace, cfg.K8s.DeploymentName)
		}
	}

	// ── Runtime engine ────────────────────────────────────────────────────────
	evalCfg := runtime.EvaluatorConfig{
		UpHysteresis:   cfg.Evaluator.UpHysteresis,
		DownHysteresis: cfg.Evaluator.DownHysteresis,
		TickInterval:   cfg.SignalBus.TickInterval,
	}
	engine := runtime.NewEngine(bundle, fetchers, nil, evalCfg)

	// ── Register tier transition handlers ─────────────────────────────────────
	engine.RegisterHandler(func(t runtime.TierTransition) {
		// 1. Audit log.
		auditLog.Log(audit.Event{
			Type:     audit.EventTierTransition,
			Service:  bundle.Service,
			FromTier: t.From,
			ToTier:   t.To,
			Signals:  t.Signals,
		})

		// 2. Print to terminal.
		printTransition(t)

		// 3. Update sidecar immediately so feature flags change right away.
		sidecarSrv.SetTier(t.To, t.Signals)
		fmt.Printf("[sidecar] flags updated → tier=%s\n", t.To)

		// 4. Write queue: activate on degraded tiers, drain on recovery.
		if writeQueue != nil {
			if tierHasQueueWrites(bundle, t.To) {
				fmt.Printf("[queue]   write buffering ACTIVE (depth=0 → max=%d)\n", writeQueue.MaxDepth())
			} else if tierHasQueueWrites(bundle, t.From) {
				// Recovering from a queued tier — drain the buffer.
				writes := writeQueue.Drain()
				if len(writes) > 0 {
					fmt.Printf("[queue]   draining %d buffered writes on recovery to %s\n", len(writes), t.To)
					// In a real system, replay writes against the backend here.
				}
			}
		}

		// 5. K8s reconciler — apply replica/scheduling policy.
		if k8sReconciler != nil {
			go func(transition runtime.TierTransition) {
				ctx2, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := k8sReconciler.Reconcile(ctx2, transition.To); err != nil {
					fmt.Fprintf(os.Stderr, "[k8s] reconcile tier=%s: %v\n", transition.To, err)
				}
			}(t)
		}

		// 5. Human gate — fire in a separate goroutine so the dispatcher
		//    is never blocked waiting for an operator response.
		if spec := tierGateSpec(bundle, t.To); spec != nil {
			go func(transition runtime.TierTransition, gateSpec *policy.GateSpec) {
				printGateInstructions(cfg.Gate.SlackWebhookURL, cfg.Gate.CallbackBaseURL, transition.To)

				gateCtx, cancel := context.WithTimeout(
					context.Background(),
					gateSpec.Wait+gateTimeout,
				)
				defer cancel()

				outcome, overrideTier := humanGate.Fire(gateCtx, bundle.Service, gateSpec, transition.Signals)
				fmt.Printf("[gate] outcome=%s\n", outcome)

				if overrideTier != "" && overrideTier != transition.To {
					// Operator chose a different tier — lock the evaluator to it.
					lockDuration := time.Duration(0)
					if overrideTier != "" {
						// Default lock: 10 minutes. Gate override holds until operator re-enables.
						lockDuration = 10 * time.Minute
					}
					engine.LockTier(overrideTier, lockDuration)
					sidecarSrv.SetTier(overrideTier, transition.Signals)
					fmt.Printf("[sidecar] override → tier=%s (locked 10m)\n", overrideTier)
				}
			}(t, spec)
		}
	})

	// ── Context — cancelled by SIGINT/SIGTERM ─────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── Start all servers in background goroutines ────────────────────────────

	startServer("[gate-callback]", ":9292", gate.NewCallbackServer(":9292", humanGate).Start, ctx)
	startHTTPServer("[sidecar]", ":8081", sidecarSrv.Start, ctx)

	injectSrv := newInjectServer(":9191", store, writeQueue, engine)
	go serveHTTP(injectSrv, ctx)
	fmt.Println("[inject]   listening on :9191  (POST /inject | POST /write | GET /signals)")

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Metrics.PrometheusPort),
		Handler: metricsMux,
	}
	go serveHTTP(metricsSrv, ctx)
	fmt.Printf("[metrics]  listening on :%d/metrics\n", cfg.Metrics.PrometheusPort)

	// ── Start engine (blocks until ctx is cancelled) ──────────────────────────
	fmt.Printf("\n[engine]   started  service=%s  tiers=%d\n", bundle.Service, len(bundle.Tiers))
	fmt.Printf("[engine]   current tier: %s\n\n", engine.CurrentTier())
	engine.Start(ctx)

	fmt.Println("\n[engine]   shut down cleanly")
	return nil
}

// ─── tier gate lookup ─────────────────────────────────────────────────────────

// tierGateSpec returns the GateSpec for the named tier, or nil if there is none.
func tierGateSpec(bundle *compiler.CompiledBundle, tierName string) *policy.GateSpec {
	for _, t := range bundle.Tiers {
		if t.Name == tierName {
			return t.GateSpec
		}
	}
	return nil
}

// ─── server helpers ───────────────────────────────────────────────────────────

type contextStarter func(ctx context.Context) error

// startServer launches a contextStarter (e.g. gate.CallbackServer.Start) in a goroutine.
func startServer(label, addr string, fn contextStarter, ctx context.Context) {
	fmt.Printf("%s listening on %s\n", label, addr)
	go func() {
		if err := fn(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "%s error: %v\n", label, err)
		}
	}()
}

// startHTTPServer is like startServer but for the sidecar which uses the same interface.
func startHTTPServer(label, addr string, fn contextStarter, ctx context.Context) {
	fmt.Printf("%s listening on %s\n", label, addr)
	go func() {
		if err := fn(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "%s error: %v\n", label, err)
		}
	}()
}

// serveHTTP starts an http.Server and shuts it down gracefully when ctx is cancelled.
func serveHTTP(srv *http.Server, ctx context.Context) {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server %s error: %v\n", srv.Addr, err)
	}
}

// ─── inject server ────────────────────────────────────────────────────────────

// newInjectServer creates an HTTP server for local signal injection.
//
//	POST /inject        body: {"rps": 2500, "pod_ceiling": 0.9}   — set signal values
//	GET  /signals                                                  — view current signal values
//	POST /write         body: {"id":"w1","payload":"..."}          — enqueue a write (when buffering active)
//	GET  /write/depth                                              — current queue depth
//	POST /override      body: {"tier":"nominal","duration_min":10} — manual tier lock
func newInjectServer(addr string, store *sigfetch.InjectedStore, wq *writequeue.Queue, eng *runtime.Engine) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var vals map[string]float64
		if err := json.NewDecoder(r.Body).Decode(&vals); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		store.Set(vals)

		fmt.Printf("[inject]   ")
		for k, v := range vals {
			fmt.Printf("%s=%.2f  ", k, v)
		}
		fmt.Println()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"injected": vals,
			"ts":       time.Now().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/signals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.Get())
	})

	// Write queue endpoints (only meaningful when a tier with queue_writes is active).
	if wq != nil {
		mux.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				ID      string `json:"id"`
				Payload string `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			entry := writequeue.Write{
				ID:      req.ID,
				Payload: []byte(req.Payload),
			}
			if err := wq.Enqueue(entry); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"queued": req.ID,
				"depth":  wq.Depth(),
			})
		})

		mux.HandleFunc("/write/depth", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"depth":   wq.Depth(),
				"dropped": wq.Dropped(),
			})
		})
	}

	// Manual override endpoint: lock the evaluator to a tier for N minutes.
	if eng != nil {
		mux.HandleFunc("/override", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				Tier        string `json:"tier"`
				DurationMin int    `json:"duration_min"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if req.DurationMin <= 0 {
				req.DurationMin = 10
			}
			dur := time.Duration(req.DurationMin) * time.Minute
			eng.LockTier(req.Tier, dur)
			fmt.Printf("[override] tier=%s locked for %dm\n", req.Tier, req.DurationMin)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"locked_tier":    req.Tier,
				"duration_min":   req.DurationMin,
				"locked_until":   time.Now().Add(dur).Format(time.RFC3339),
			})
		})
	}

	return &http.Server{Addr: addr, Handler: mux}
}

// ─── display helpers ──────────────────────────────────────────────────────────

func printBanner(b *compiler.CompiledBundle) {
	fmt.Printf("╔══════════════════════════════════════════════════╗\n")
	fmt.Printf("║  IDDC — Intent-Driven Degradation Contracts      ║\n")
	fmt.Printf("╚══════════════════════════════════════════════════╝\n")
	fmt.Printf("  service : %s\n", b.Service)
	fmt.Printf("  version : %s\n", b.Version)
	fmt.Printf("  tiers   : %d\n", len(b.Tiers))
	fmt.Printf("  signals : %d\n\n", len(b.SignalSources))
	fmt.Println("  Tier ladder:")
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
func printGateInstructions(slackURL, callbackBase, tier string) {
	if slackURL != "" {
		return // Slack will handle notification
	}
	base := callbackBase
	if base == "" {
		base = "http://localhost:9292"
	}
	fmt.Printf("\n╔══════════════════════════════════════════════════╗\n")
	fmt.Printf("║  GATE FIRED — tier: %-28s║\n", tier)
	fmt.Printf("╠══════════════════════════════════════════════════╣\n")
	fmt.Printf("║  Approve :  POST %s/gate/approve\n", base)
	fmt.Printf("║             body: {\"tier\":\"%s\"}\n", tier)
	fmt.Printf("║\n")
	fmt.Printf("║  Override:  POST %s/gate/override\n", base)
	fmt.Printf("║             body: {\"target_tier\":\"nominal\",\"duration_min\":10}\n")
	fmt.Printf("║\n")
	fmt.Printf("║  Escalate:  POST %s/gate/escalate\n", base)
	fmt.Printf("║             body: {\"message\":\"paging SRE\"}\n")
	fmt.Printf("╚══════════════════════════════════════════════════╝\n\n")
}

func printTransition(t runtime.TierTransition) {
	ts := time.Now().Format("15:04:05")
	fmt.Printf("\n┌─ TIER TRANSITION [%s] ─────────────────────────\n", ts)
	fmt.Printf("│  %s  →  %s\n", t.From, t.To)
	if len(t.Signals) > 0 {
		fmt.Printf("│  signals: ")
		for k, v := range t.Signals {
			fmt.Printf("%s=%.2f  ", k, v)
		}
		fmt.Println()
	}
	fmt.Printf("└───────────────────────────────────────────────────\n")
}

// ─── config helpers ───────────────────────────────────────────────────────────

// prometheusQueryForSignal returns a default PromQL expression for known signal IDs.
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

// ─── write queue helpers ──────────────────────────────────────────────────────

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

// tierHasQueueWrites reports whether the named tier has queue_writes configured.
func tierHasQueueWrites(bundle *compiler.CompiledBundle, tierName string) bool {
	for _, t := range bundle.Tiers {
		if t.Name == tierName {
			return t.Behavior.QueueWrites != nil
		}
	}
	return false
}
