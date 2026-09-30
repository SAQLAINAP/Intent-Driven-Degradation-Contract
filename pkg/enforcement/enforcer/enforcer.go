// Package enforcer turns evaluator tier proposals into enforced tiers.
//
// The evaluator decides which tier the signals call for; the enforcer decides
// which tier is actually applied (sidecar flags, write queue, Kubernetes). The
// two differ while a human gate is open: an upward move into a tier that
// declares human_gate is held at the current tier until an operator answers
// or the gate's on_timeout policy runs.
//
//	approve                    → apply the gated tier
//	override {target, minutes} → apply target and lock the evaluator to it
//	escalate                   → apply the most degraded tier and lock it
//	timeout + execute_policy   → apply the gated tier
//	timeout + hold             → stay on the current tier and lock it
//	timeout + escalate         → apply the most degraded tier and lock it
//	signals move elsewhere     → gate is superseded; the new proposal is handled
//
// Downward moves (recovery) and moves into ungated tiers apply immediately.
package enforcer

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/audit"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/compiler"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/sidecar"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/writequeue"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/gate"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/metrics"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/runtime"
)

// DefaultHoldDuration is how long the evaluator is locked after an override
// without an explicit duration, an escalation, or an on_timeout: hold.
const DefaultHoldDuration = 10 * time.Minute

// Gate is the subset of gate.HumanGate the enforcer needs.
type Gate interface {
	Fire(ctx context.Context, svc, fromTier, tier string, spec *policy.GateSpec, signals map[string]float64) gate.Decision
}

// Locker pins the evaluator to a tier (implemented by runtime.Engine).
type Locker interface {
	LockTier(tier string, d time.Duration) error
	Unlock()
}

// Reconciler applies a tier to external infrastructure (e.g. Kubernetes).
type Reconciler interface {
	Reconcile(ctx context.Context, tier string) error
}

// Config wires the enforcer to its collaborators. Queue and Reconciler are optional.
type Config struct {
	Bundle     *compiler.CompiledBundle
	Sidecar    *sidecar.Server
	Queue      *writequeue.Queue
	Reconciler Reconciler
	Gate       Gate
	Locker     Locker
	Audit      *audit.Logger
	Out        io.Writer // operator-facing log lines; nil discards them

	HoldDuration time.Duration // 0 = DefaultHoldDuration

	// OnGateOpen is called (without locks held) when a gate starts holding a transition.
	OnGateOpen func(from, to string, spec *policy.GateSpec)
}

// State is a snapshot of what the enforcer is currently applying.
type State struct {
	Tier        string    `json:"tier"`
	Mode        string    `json:"mode"`
	Since       time.Time `json:"since"`
	PendingTier string    `json:"pending_tier,omitempty"` // tier held behind an open gate
	QueueActive bool      `json:"queue_active"`
}

// Enforcer applies tiers. All methods are safe for concurrent use.
type Enforcer struct {
	cfg Config

	mu          sync.Mutex
	applied     string
	appliedIdx  int
	since       time.Time
	lastSignals map[string]float64

	gateSeq     uint64
	gateCancel  context.CancelFunc
	pendingTier string
}

// New creates an enforcer starting at the policy's first (least degraded) tier.
func New(cfg Config) *Enforcer {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.HoldDuration <= 0 {
		cfg.HoldDuration = DefaultHoldDuration
	}
	e := &Enforcer{cfg: cfg, since: time.Now()}
	if len(cfg.Bundle.Tiers) > 0 {
		e.applied = cfg.Bundle.Tiers[0].Name
	}
	cfg.Sidecar.UsePolicy(sidecar.PolicyFromBundle(cfg.Bundle))
	metrics.SetCurrentTier(cfg.Bundle.Service, e.applied, 0)
	return e
}

// HandleTransition is registered with the runtime engine's dispatcher.
func (e *Enforcer) HandleTransition(t runtime.TierTransition) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastSignals = t.Signals
	e.cancelGateLocked()

	if t.To == e.applied {
		return // e.g. signals fell back while a gate was holding a higher tier
	}
	if spec := e.gateSpec(t.To); spec != nil && e.index(t.To) > e.appliedIdx {
		e.openGateLocked(t.To, spec, t.Signals)
		return
	}
	e.applyLocked(t.To, t.Signals, "signals")
}

// Force applies tier immediately and locks the evaluator to it for d,
// cancelling any open gate. Used by the manual override API.
func (e *Enforcer) Force(tier string, d time.Duration, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.index(tier) < 0 {
		return fmt.Errorf("unknown tier %q", tier)
	}
	if d <= 0 {
		d = e.cfg.HoldDuration
	}
	if err := e.cfg.Locker.LockTier(tier, d); err != nil {
		return err
	}
	e.cancelGateLocked()
	override := tier
	e.cfg.Audit.Log(audit.Event{
		Type:         audit.EventTierOverride,
		Service:      e.cfg.Bundle.Service,
		FromTier:     e.applied,
		ToTier:       tier,
		OverrideTier: &override,
		Reason:       reason,
	})
	fmt.Fprintf(e.cfg.Out, "[override] tier=%s locked for %s (%s)\n", tier, d, reason)
	e.applyLocked(tier, e.lastSignals, reason)
	return nil
}

// Release removes any evaluator lock so signals drive the tier again.
func (e *Enforcer) Release() {
	e.cfg.Locker.Unlock()
	fmt.Fprintln(e.cfg.Out, "[override] lock released — signals drive the tier again")
}

// State returns the applied tier and any gate-held tier.
func (e *Enforcer) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return State{
		Tier:        e.applied,
		Mode:        e.mode(e.applied),
		Since:       e.since,
		PendingTier: e.pendingTier,
		QueueActive: e.cfg.Queue != nil && e.queueTier(e.applied),
	}
}

// QueueActive reports whether the applied tier buffers writes.
func (e *Enforcer) QueueActive() bool {
	return e.State().QueueActive
}

// ─── internals (call with e.mu held) ─────────────────────────────────────────

func (e *Enforcer) openGateLocked(tier string, spec *policy.GateSpec, signals map[string]float64) {
	e.gateSeq++
	seq := e.gateSeq
	ctx, cancel := context.WithCancel(context.Background())
	e.gateCancel = cancel
	e.pendingTier = tier
	from := e.applied

	onTimeout := spec.OnTimeout
	if onTimeout == "" {
		onTimeout = "execute_policy"
	}
	fmt.Fprintf(e.cfg.Out, "[gate]    %s → %s HELD for operator decision (wait=%s, on_timeout=%s)\n",
		from, tier, spec.Wait, onTimeout)

	go func() {
		if e.cfg.OnGateOpen != nil {
			e.cfg.OnGateOpen(from, tier, spec)
		}
		d := e.cfg.Gate.Fire(ctx, e.cfg.Bundle.Service, from, tier, spec, signals)
		cancel()
		e.resolveGate(seq, tier, spec, signals, d)
	}()
}

func (e *Enforcer) resolveGate(seq uint64, tier string, spec *policy.GateSpec, signals map[string]float64, d gate.Decision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if seq != e.gateSeq || e.pendingTier == "" {
		return // superseded by a newer transition or override
	}
	e.gateCancel = nil
	e.pendingTier = ""
	fmt.Fprintf(e.cfg.Out, "[gate]    %s gate closed: %s\n", tier, d.Outcome)

	switch d.Outcome {
	case gate.OutcomeApproved:
		e.applyLocked(tier, signals, "gate approved")
	case gate.OutcomeOverridden:
		if e.index(d.TargetTier) < 0 {
			fmt.Fprintf(e.cfg.Out, "[gate]    override target %q is not a tier — holding at %s\n", d.TargetTier, e.applied)
			e.lockLocked(e.applied, e.cfg.HoldDuration)
			return
		}
		dur := d.Duration
		if dur <= 0 {
			dur = e.cfg.HoldDuration
		}
		e.lockLocked(d.TargetTier, dur)
		e.applyLocked(d.TargetTier, signals, "gate override")
	case gate.OutcomeEscalated:
		e.escalateLocked(signals, "gate escalated")
	case gate.OutcomeTimedOut:
		switch spec.OnTimeout {
		case "hold":
			fmt.Fprintf(e.cfg.Out, "[gate]    on_timeout=hold — staying at %s for %s\n", e.applied, e.cfg.HoldDuration)
			e.lockLocked(e.applied, e.cfg.HoldDuration)
		case "escalate":
			e.escalateLocked(signals, "gate timeout: escalate")
		default:
			e.applyLocked(tier, signals, "gate timeout: execute_policy")
		}
	}
}

func (e *Enforcer) escalateLocked(signals map[string]float64, reason string) {
	top := e.cfg.Bundle.Tiers[len(e.cfg.Bundle.Tiers)-1].Name
	e.lockLocked(top, e.cfg.HoldDuration)
	e.applyLocked(top, signals, reason)
}

func (e *Enforcer) lockLocked(tier string, d time.Duration) {
	if err := e.cfg.Locker.LockTier(tier, d); err != nil {
		fmt.Fprintf(e.cfg.Out, "[gate]    lock %s: %v\n", tier, err)
	}
}

func (e *Enforcer) cancelGateLocked() {
	if e.gateCancel != nil {
		e.gateCancel()
		e.gateCancel = nil
	}
	e.pendingTier = ""
}

func (e *Enforcer) applyLocked(to string, signals map[string]float64, reason string) {
	if to == e.applied {
		return
	}
	from := e.applied
	e.applied = to
	e.appliedIdx = e.index(to)
	e.since = time.Now()
	svc := e.cfg.Bundle.Service

	e.cfg.Audit.Log(audit.Event{
		Type:     audit.EventTierTransition,
		Service:  svc,
		FromTier: from,
		ToTier:   to,
		Signals:  signals,
		Reason:   reason,
	})
	metrics.TierTransitionsTotal.WithLabelValues(svc, from, to).Inc()
	metrics.SetCurrentTier(svc, to, e.appliedIdx)
	printTransition(e.cfg.Out, from, to, reason, signals)

	e.cfg.Sidecar.SetTier(to, signals)
	fmt.Fprintf(e.cfg.Out, "[sidecar] flags updated → tier=%s\n", to)

	if q := e.cfg.Queue; q != nil {
		switch {
		case e.queueTier(to):
			fmt.Fprintf(e.cfg.Out, "[queue]   write buffering ACTIVE (depth=%d, max=%d)\n", q.Depth(), q.MaxDepth())
		case e.queueTier(from):
			// Recovering from a queued tier — drain the buffer. In a real system
			// the drained writes are replayed against the backend here.
			writes := q.Drain()
			fmt.Fprintf(e.cfg.Out, "[queue]   drained %d buffered writes on recovery to %s\n", len(writes), to)
			e.cfg.Audit.Log(audit.Event{Type: audit.EventQueueDrained, Service: svc, Tier: to, Count: len(writes)})
		}
	}

	if r := e.cfg.Reconciler; r != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := r.Reconcile(ctx, to); err != nil {
				fmt.Fprintf(e.cfg.Out, "[k8s]     reconcile tier=%s: %v\n", to, err)
			}
		}()
	}
}

func (e *Enforcer) index(name string) int {
	for i, t := range e.cfg.Bundle.Tiers {
		if t.Name == name {
			return i
		}
	}
	return -1
}

func (e *Enforcer) tier(name string) *compiler.CompiledTier {
	if i := e.index(name); i >= 0 {
		return &e.cfg.Bundle.Tiers[i]
	}
	return nil
}

func (e *Enforcer) gateSpec(name string) *policy.GateSpec {
	if t := e.tier(name); t != nil {
		return t.GateSpec
	}
	return nil
}

func (e *Enforcer) queueTier(name string) bool {
	t := e.tier(name)
	return t != nil && t.Behavior.QueueWrites != nil
}

func (e *Enforcer) mode(name string) string {
	if t := e.tier(name); t != nil && t.Behavior.Mode != "" {
		return t.Behavior.Mode
	}
	return "full_service"
}

func printTransition(w io.Writer, from, to, reason string, signals map[string]float64) {
	fmt.Fprintf(w, "\n┌─ TIER TRANSITION [%s] ─────────────────────────\n", time.Now().Format("15:04:05"))
	fmt.Fprintf(w, "│  %s  →  %s   (%s)\n", from, to, reason)
	if len(signals) > 0 {
		ids := make([]string, 0, len(signals))
		for id := range signals {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Fprintf(w, "│  signals: ")
		for _, id := range ids {
			fmt.Fprintf(w, "%s=%.2f  ", id, signals[id])
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "└───────────────────────────────────────────────────\n")
}
