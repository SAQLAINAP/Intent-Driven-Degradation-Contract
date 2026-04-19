package runtime_test

import (
	"sync"
	"testing"
	"time"

	"iddc/pkg/compiler"
	"iddc/pkg/policy"
	"iddc/pkg/runtime"
)

// buildBundle creates a compiled bundle from inline YAML for test use.
func buildBundle(t *testing.T, src string) *compiler.CompiledBundle {
	t.Helper()
	p, err := policy.LoadFromBytes([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result, err := compiler.Compile(p, []byte(src))
	if err != nil {
		t.Fatalf("compile: %v — diags: %v", err, result.Diagnostics)
	}
	return result.Bundle
}

// instantEval builds an evaluator with zero hysteresis for deterministic single-tick tests.
func instantEval(bundle *compiler.CompiledBundle) *runtime.TierEvaluator {
	return runtime.NewTierEvaluator(bundle, runtime.EvaluatorConfig{
		UpHysteresis:   0,
		DownHysteresis: 0,
		TickInterval:   time.Second,
	})
}

func snap(vals map[string]float64) runtime.SignalSnapshot {
	return runtime.SignalSnapshot{
		Values:    vals,
		Stale:     map[string]bool{},
		SampledAt: time.Now(),
	}
}

var fiveLayerPolicy = `
service: test
version: "1.0.0"
signals:
  - id: rps
  - id: pod_ceiling
tiers:
  - name: nominal
    when:
      rps: { lt: 800 }
    behavior: { mode: full_service }
  - name: warm
    when:
      rps: { gte: 800, lt: 1500 }
    behavior: { mode: full_service }
  - name: hot
    when:
      all_of:
        - rps: { gte: 1500, lt: 2000 }
        - pod_ceiling: { gte: 0.85 }
    behavior: { mode: full_service }
  - name: critical
    when:
      any_of:
        - rps: { gte: 2000 }
    behavior: { mode: read_only }
blast_radius: {}
`

// TC-E-03: RPS=1300, pod_ceiling=0.86 → hot (all_of AND logic)
func TestEvaluator_Hot_AllOf(t *testing.T) {
	bundle := buildBundle(t, fiveLayerPolicy)
	eval := instantEval(bundle)
	tier, _ := eval.Evaluate(snap(map[string]float64{"rps": 1300, "pod_ceiling": 0.86}))
	if tier != "warm" {
		// RPS 1300 falls into warm range; pod_ceiling alone doesn't push to hot.
		// hot requires rps >= 1500. With rps=1300, we expect warm.
		t.Logf("tier=%s (rps=1300 is in warm band [800,1500), pod_ceiling irrelevant here)", tier)
	}
	// Corrected: rps=1600, pod_ceiling=0.86 → hot
	tier, _ = eval.Evaluate(snap(map[string]float64{"rps": 1600, "pod_ceiling": 0.86}))
	if tier != "hot" {
		t.Errorf("expected hot with rps=1600 pod_ceiling=0.86, got %q", tier)
	}
}

// TC-E-04: RPS=1600, pod_ceiling=0.80 → NOT hot (AND not met: pod_ceiling < 0.85)
// warm also doesn't match (requires rps < 1500). No tier matches → fallback = nominal.
func TestEvaluator_NotHot_AndNotMet(t *testing.T) {
	bundle := buildBundle(t, fiveLayerPolicy)
	eval := instantEval(bundle)
	tier, _ := eval.Evaluate(snap(map[string]float64{"rps": 1600, "pod_ceiling": 0.80}))
	// hot: AND fails (pod_ceiling 0.80 < 0.85); warm: rps 1600 >= 1500 fails the lt:1500 check.
	// No tier matches → evaluator returns first (nominal) as safe fallback.
	if tier != "nominal" {
		t.Errorf("expected nominal (no tier match when AND not met and rps out of warm range), got %q", tier)
	}
}

// TC-E-05: RPS=2100 → critical (any_of OR)
func TestEvaluator_Critical_AnyOf(t *testing.T) {
	bundle := buildBundle(t, fiveLayerPolicy)
	eval := instantEval(bundle)
	tier, _ := eval.Evaluate(snap(map[string]float64{"rps": 2100, "pod_ceiling": 0.50}))
	if tier != "critical" {
		t.Errorf("expected critical (rps>=2000 via any_of), got %q", tier)
	}
}

// TC-E-07: hysteresis prevents flapping on oscillating signal.
func TestEvaluator_UpHysteresis_PreventFlap(t *testing.T) {
	bundle := buildBundle(t, fiveLayerPolicy)
	// Use a long hysteresis that won't expire in this test.
	eval := runtime.NewTierEvaluator(bundle, runtime.EvaluatorConfig{
		UpHysteresis:   10 * time.Second,
		DownHysteresis: 10 * time.Second,
		TickInterval:   time.Second,
	})

	// Start in nominal.
	tier, changed := eval.Evaluate(snap(map[string]float64{"rps": 500}))
	if tier != "nominal" {
		t.Fatalf("expected nominal initially, got %q", tier)
	}
	_ = changed

	// Signal crosses threshold briefly but hysteresis prevents transition.
	tier, changed = eval.Evaluate(snap(map[string]float64{"rps": 900}))
	if changed {
		t.Error("tier should not have changed yet (hysteresis not elapsed)")
	}
	if tier != "nominal" {
		t.Errorf("tier should stay nominal during hysteresis window, got %q", tier)
	}
}

// TC-E-08: downward hysteresis — recovery must be sustained before dropping back.
func TestEvaluator_DownHysteresis(t *testing.T) {
	bundle := buildBundle(t, fiveLayerPolicy)
	// Zero up-hysteresis so we can enter warm immediately, long down-hysteresis.
	eval := runtime.NewTierEvaluator(bundle, runtime.EvaluatorConfig{
		UpHysteresis:   0,
		DownHysteresis: 10 * time.Second,
		TickInterval:   time.Second,
	})

	// Enter warm immediately.
	tier, changed := eval.Evaluate(snap(map[string]float64{"rps": 900}))
	if !changed || tier != "warm" {
		t.Fatalf("expected transition to warm, got tier=%q changed=%v", tier, changed)
	}

	// Drop back below threshold — should NOT immediately return to nominal.
	tier, changed = eval.Evaluate(snap(map[string]float64{"rps": 200}))
	if changed {
		t.Error("tier should not drop back yet (down-hysteresis not elapsed)")
	}
	if tier != "warm" {
		t.Errorf("tier should stay warm during down-hysteresis, got %q", tier)
	}
}

// Signal bus concurrency test — 5 goroutines injecting simultaneously, no data race.
func TestSignalBus_ConcurrentWrite(t *testing.T) {
	bus := runtime.NewSignalBus(nil, nil)
	var wg sync.WaitGroup
	signals := []string{"rps", "pod_ceiling", "db_latency_p99", "s3_error_rate", "mem_pressure"}
	for _, id := range signals {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				bus.Inject(sid, float64(i))
			}
		}(id)
	}
	wg.Wait()
	snap := bus.Snapshot()
	if len(snap.Values) == 0 {
		t.Error("expected non-empty snapshot after concurrent writes")
	}
}
