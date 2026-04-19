package runtime

import (
	"sync"
	"time"

	"iddc/pkg/compiler"
	"iddc/pkg/policy"
)

// EvaluatorConfig holds hysteresis durations.
type EvaluatorConfig struct {
	UpHysteresis   time.Duration // minimum time signal must breach before tier changes up
	DownHysteresis time.Duration // minimum time signal must recover before tier changes down
	TickInterval   time.Duration // how often the evaluator checks signals
}

var DefaultEvaluatorConfig = EvaluatorConfig{
	UpHysteresis:   30 * time.Second,
	DownHysteresis: 90 * time.Second,
	TickInterval:   2 * time.Second,
}

// TierEvaluator walks the compiled tier list on each tick and applies hysteresis.
type TierEvaluator struct {
	bundle *compiler.CompiledBundle
	cfg    EvaluatorConfig

	mu             sync.Mutex
	currentTier    string
	currentTierIdx int
	candidateTier  string
	candidateStart time.Time
	candidateUp    bool // true = candidate is a higher tier than current

	// TierLock: when set, the evaluator holds this tier until lockUntil.
	lockedTier string
	lockUntil  time.Time
}

// NewTierEvaluator creates an evaluator for the given compiled policy.
func NewTierEvaluator(bundle *compiler.CompiledBundle, cfg EvaluatorConfig) *TierEvaluator {
	nominalTier := ""
	if len(bundle.Tiers) > 0 {
		nominalTier = bundle.Tiers[0].Name
	}
	return &TierEvaluator{
		bundle:      bundle,
		cfg:         cfg,
		currentTier: nominalTier,
	}
}

// LockTier forces the evaluator to hold tierName for duration, ignoring signals.
// This implements the override hold (Phase 3.3).
func (e *TierEvaluator) LockTier(tierName string, duration time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lockedTier = tierName
	e.lockUntil = time.Now().Add(duration)
	e.currentTier = tierName
	e.currentTierIdx = e.tierIndex(tierName)
	e.candidateTier = ""
}

// Evaluate takes a snapshot of current signal values and returns:
//   - newTier: the tier name that should be active (may equal current tier)
//   - changed: true only when a confirmed tier transition occurred
func (e *TierEvaluator) Evaluate(snap SignalSnapshot) (newTier string, changed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	// Respect a tier lock (manual override hold).
	if e.lockedTier != "" && now.Before(e.lockUntil) {
		return e.currentTier, false
	}
	if e.lockedTier != "" {
		// Lock expired — clear it and let normal evaluation resume.
		e.lockedTier = ""
	}

	candidate := e.matchTierLocked(snap)

	if candidate == e.currentTier {
		// Reset any pending candidate.
		e.candidateTier = ""
		return e.currentTier, false
	}

	// Determine direction of change.
	candidateIdx := e.tierIndex(candidate)
	isUpward := candidateIdx > e.currentTierIdx

	if e.candidateTier != candidate {
		// New candidate — start the hysteresis clock.
		e.candidateTier = candidate
		e.candidateStart = now
		e.candidateUp = isUpward
		// If hysteresis is zero, fall through immediately to the elapsed check below.
		// Otherwise return and wait for the next tick.
		hysteresisCheck := e.cfg.DownHysteresis
		if isUpward {
			hysteresisCheck = e.cfg.UpHysteresis
		}
		if hysteresisCheck > 0 {
			return e.currentTier, false
		}
	}

	// Same candidate — check if hysteresis window has elapsed.
	hysteresis := e.cfg.DownHysteresis
	if e.candidateUp {
		hysteresis = e.cfg.UpHysteresis
	}

	if now.Sub(e.candidateStart) >= hysteresis {
		// Transition confirmed.
		prev := e.currentTier
		e.currentTier = candidate
		e.currentTierIdx = candidateIdx
		e.candidateTier = ""
		_ = prev
		return e.currentTier, true
	}

	return e.currentTier, false
}

// CurrentTier returns the currently active tier name (safe to call from any goroutine).
func (e *TierEvaluator) CurrentTier() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentTier
}

// Bundle returns the compiled policy bundle this evaluator was built from.
func (e *TierEvaluator) Bundle() *compiler.CompiledBundle {
	return e.bundle
}

// matchTierLocked walks the tier list in order and returns the name of the first
// tier whose condition is satisfied by the snapshot.
// Falls back to the first (lowest) tier if nothing matches.
// Must be called with e.mu held.
func (e *TierEvaluator) matchTierLocked(snap SignalSnapshot) string {
	for _, t := range e.bundle.Tiers {
		if evalCondition(t.Condition, snap.Values) {
			return t.Name
		}
	}
	if len(e.bundle.Tiers) > 0 {
		return e.bundle.Tiers[0].Name
	}
	return ""
}

func (e *TierEvaluator) tierIndex(name string) int {
	for i, t := range e.bundle.Tiers {
		if t.Name == name {
			return i
		}
	}
	return 0
}

// evalCondition recursively evaluates the Condition tree against a signal snapshot.
func evalCondition(c policy.Condition, vals map[string]float64) bool {
	// AllOf (AND)
	if len(c.AllOf) > 0 {
		for _, sub := range c.AllOf {
			if !evalCondition(sub, vals) {
				return false
			}
		}
		return true
	}

	// AnyOf (OR)
	if len(c.AnyOf) > 0 {
		for _, sub := range c.AnyOf {
			if evalCondition(sub, vals) {
				return true
			}
		}
		return false
	}

	// Flat comparisons: ALL must be true (implicit AND for multi-key flat conditions).
	for signalID, cmp := range c.Comparisons {
		v, ok := vals[signalID]
		if !ok {
			return false
		}
		if !evalComparator(v, cmp) {
			return false
		}
	}

	// Empty condition matches anything (e.g. nominal catch-all).
	return true
}

func evalComparator(v float64, c policy.Comparator) bool {
	if c.Lt != nil && !(v < *c.Lt) {
		return false
	}
	if c.Lte != nil && !(v <= *c.Lte) {
		return false
	}
	if c.Gt != nil && !(v > *c.Gt) {
		return false
	}
	if c.Gte != nil && !(v >= *c.Gte) {
		return false
	}
	if c.Eq != nil && v != *c.Eq {
		return false
	}
	return true
}
