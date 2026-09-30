package enforcer_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/audit"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/compiler"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/enforcer"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/sidecar"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/writequeue"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/gate"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/runtime"
)

const ladder = `
service: shop
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: nominal
    when: { rps: { lt: 800 } }
    behavior: { mode: full_service }
  - name: warm
    when: { rps: { gte: 800, lt: 2000 } }
    behavior: { mode: full_service, disable: [recommendations] }
  - name: critical
    when: { rps: { gte: 2000, lt: 5000 } }
    human_gate: { wait: 1m, on_timeout: %s }
    behavior:
      mode: read_only
      queue_writes: { max_depth: 10, ttl: 1m }
  - name: survival
    when: { rps: { gte: 5000 } }
    behavior: { mode: static_fallback }
`

// fakeGate lets a test decide when and how a gate closes.
type fakeGate struct {
	opened chan string
	answer chan gate.Decision
}

func (f *fakeGate) Fire(ctx context.Context, svc, from, tier string, spec *policy.GateSpec, sig map[string]float64) gate.Decision {
	f.opened <- tier
	select {
	case d := <-f.answer:
		return d
	case <-ctx.Done():
		return gate.Decision{Outcome: gate.OutcomeSuperseded}
	}
}

type fakeLocker struct {
	mu     sync.Mutex
	tier   string
	dur    time.Duration
	unlock int
}

func (l *fakeLocker) LockTier(tier string, d time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tier, l.dur = tier, d
	return nil
}

func (l *fakeLocker) Unlock() { l.mu.Lock(); l.unlock++; l.mu.Unlock() }

func (l *fakeLocker) get() (string, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tier, l.dur
}

type harness struct {
	e      *enforcer.Enforcer
	gate   *fakeGate
	lock   *fakeLocker
	side   *sidecar.Server
	queue  *writequeue.Queue
	events *bytes.Buffer
}

func setup(t *testing.T, onTimeout string) *harness {
	t.Helper()
	src := []byte(fmt.Sprintf(ladder, onTimeout))
	p, err := policy.LoadFromBytes(src)
	if err != nil {
		t.Fatal(err)
	}
	res, err := compiler.Compile(p, src)
	if err != nil {
		t.Fatalf("compile: %v %v", err, res.Diagnostics)
	}
	h := &harness{
		gate:   &fakeGate{opened: make(chan string, 4), answer: make(chan gate.Decision, 1)},
		lock:   &fakeLocker{},
		side:   sidecar.NewServer(":0", ""),
		queue:  writequeue.NewQueue("shop", 10, time.Minute),
		events: &bytes.Buffer{},
	}
	h.e = enforcer.New(enforcer.Config{
		Bundle:  res.Bundle,
		Sidecar: h.side,
		Queue:   h.queue,
		Gate:    h.gate,
		Locker:  h.lock,
		Audit:   audit.NewLogger(h.events),
	})
	return h
}

func (h *harness) move(from, to string) {
	h.e.HandleTransition(runtime.TierTransition{From: from, To: to, Signals: map[string]float64{}})
}

func (h *harness) waitOpened(t *testing.T, tier string) {
	t.Helper()
	select {
	case got := <-h.gate.opened:
		if got != tier {
			t.Fatalf("gate opened for %q, want %q", got, tier)
		}
	case <-time.After(time.Second):
		t.Fatal("gate never opened")
	}
}

func (h *harness) waitApplied(t *testing.T, tier string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for h.e.State().Tier != tier {
		if time.Now().After(deadline) {
			t.Fatalf("applied tier = %q, want %q", h.e.State().Tier, tier)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartsAtFirstTierWithPolicyFlags(t *testing.T) {
	h := setup(t, "execute_policy")
	if st := h.e.State(); st.Tier != "nominal" || st.Mode != "full_service" {
		t.Errorf("initial state = %+v", st)
	}
	if f := h.side.Features(); f.Tier != "nominal" || !f.Features["recommendations"] {
		t.Errorf("initial flags = %+v", f)
	}
}

func TestUngatedTransitionAppliesImmediately(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "warm")
	if st := h.e.State(); st.Tier != "warm" {
		t.Fatalf("applied = %q, want warm", st.Tier)
	}
	if f := h.side.Features(); f.Features["recommendations"] {
		t.Error("recommendations should be disabled in warm (from the policy's disable list)")
	}
}

func TestGatedTransitionIsHeldUntilApproved(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "warm")
	h.move("warm", "critical")
	h.waitOpened(t, "critical")

	st := h.e.State()
	if st.Tier != "warm" || st.PendingTier != "critical" {
		t.Fatalf("while gate open: %+v — gated tier must not be enforced yet", st)
	}
	if h.side.Tier() != "warm" {
		t.Errorf("sidecar tier = %q while gate open, want warm", h.side.Tier())
	}

	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeApproved}
	h.waitApplied(t, "critical")
	if f := h.side.Features(); f.Mode != "read_only" {
		t.Errorf("flags after approval = %+v", f)
	}
	if !h.e.QueueActive() {
		t.Error("write queue should be active in critical")
	}
}

func TestTimeoutExecutePolicyApplies(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "critical")
	h.waitOpened(t, "critical")
	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeTimedOut}
	h.waitApplied(t, "critical")
}

func TestTimeoutHoldStaysAndLocks(t *testing.T) {
	h := setup(t, "hold")
	h.move("nominal", "warm")
	h.move("warm", "critical")
	h.waitOpened(t, "critical")
	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeTimedOut}

	deadline := time.Now().Add(time.Second)
	for {
		if tier, _ := h.lock.get(); tier == "warm" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hold did not lock the evaluator to warm")
		}
		time.Sleep(time.Millisecond)
	}
	if st := h.e.State(); st.Tier != "warm" || st.PendingTier != "" {
		t.Errorf("after hold: %+v", st)
	}
}

func TestTimeoutEscalateGoesToMostDegradedTier(t *testing.T) {
	h := setup(t, "escalate")
	h.move("nominal", "critical")
	h.waitOpened(t, "critical")
	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeTimedOut}
	h.waitApplied(t, "survival")
	if tier, d := h.lock.get(); tier != "survival" || d != enforcer.DefaultHoldDuration {
		t.Errorf("lock = %s for %s", tier, d)
	}
	if !h.side.FallbackActive() {
		t.Error("static fallback should be active in survival")
	}
}

func TestOverrideAppliesTargetWithRequestedDuration(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "critical")
	h.waitOpened(t, "critical")
	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeOverridden, TargetTier: "warm", Duration: 3 * time.Minute}
	h.waitApplied(t, "warm")
	if tier, d := h.lock.get(); tier != "warm" || d != 3*time.Minute {
		t.Errorf("lock = %s for %s, want warm for 3m", tier, d)
	}
}

func TestOverrideToUnknownTierHolds(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "critical")
	h.waitOpened(t, "critical")
	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeOverridden, TargetTier: "banana"}
	deadline := time.Now().Add(time.Second)
	for h.e.State().PendingTier != "" {
		if time.Now().After(deadline) {
			t.Fatal("gate never resolved")
		}
		time.Sleep(time.Millisecond)
	}
	if st := h.e.State(); st.Tier != "nominal" {
		t.Errorf("applied = %q after bogus override, want nominal", st.Tier)
	}
}

func TestRecoveryWhileGateOpenSupersedesIt(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "warm")
	h.move("warm", "critical")
	h.waitOpened(t, "critical")

	h.move("critical", "nominal") // signals recovered before anyone answered
	if st := h.e.State(); st.Tier != "nominal" || st.PendingTier != "" {
		t.Fatalf("after recovery: %+v", st)
	}
	// A late approval of the superseded gate must not apply critical.
	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeApproved}
	time.Sleep(20 * time.Millisecond)
	if st := h.e.State(); st.Tier != "nominal" {
		t.Errorf("superseded gate was applied: %+v", st)
	}
}

func TestDownwardMoveIntoGatedTierIsNotGated(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "survival")
	h.move("survival", "critical")
	if st := h.e.State(); st.Tier != "critical" {
		t.Errorf("recovery survival→critical should apply immediately, got %+v", st)
	}
	select {
	case tier := <-h.gate.opened:
		t.Errorf("gate opened for recovery into %s", tier)
	default:
	}
}

func TestForceValidatesAndCancelsGate(t *testing.T) {
	h := setup(t, "execute_policy")
	if err := h.e.Force("banana", time.Minute, "test"); err == nil {
		t.Error("Force accepted an unknown tier")
	}
	h.move("nominal", "critical")
	h.waitOpened(t, "critical")
	if err := h.e.Force("warm", 2*time.Minute, "test"); err != nil {
		t.Fatal(err)
	}
	if st := h.e.State(); st.Tier != "warm" || st.PendingTier != "" {
		t.Errorf("after force: %+v", st)
	}
	if tier, d := h.lock.get(); tier != "warm" || d != 2*time.Minute {
		t.Errorf("lock = %s %s", tier, d)
	}
}

func TestQueueDrainsOnRecovery(t *testing.T) {
	h := setup(t, "execute_policy")
	h.move("nominal", "critical")
	h.waitOpened(t, "critical")
	h.gate.answer <- gate.Decision{Outcome: gate.OutcomeApproved}
	h.waitApplied(t, "critical")

	h.queue.Enqueue(writequeue.Write{ID: "w1"})
	h.queue.Enqueue(writequeue.Write{ID: "w2"})
	h.move("critical", "nominal")
	if d := h.queue.Depth(); d != 0 {
		t.Errorf("queue depth after recovery = %d, want 0", d)
	}
	if !bytes.Contains(h.events.Bytes(), []byte(`"event":"queue_drained"`)) {
		t.Errorf("missing queue_drained audit event:\n%s", h.events.String())
	}
}
