package runtime

import (
	"context"
	"time"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/compiler"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/metrics"
)

// Engine wires the SignalBus → TierEvaluator → Dispatcher together.
type Engine struct {
	bus        *SignalBus
	evaluator  *TierEvaluator
	dispatcher *Dispatcher
	cfg        EvaluatorConfig
	bundle     *compiler.CompiledBundle
}

// NewEngine constructs a ready-to-start runtime engine.
func NewEngine(
	bundle *compiler.CompiledBundle,
	fetchers map[string]SignalFetcher,
	cadences map[string]time.Duration,
	cfg EvaluatorConfig,
) *Engine {
	if cadences == nil {
		cadences = make(map[string]time.Duration, len(bundle.SignalSources))
		for _, s := range bundle.SignalSources {
			if s.PollCadence > 0 {
				cadences[s.ID] = s.PollCadence
			}
		}
	}
	return &Engine{
		bus:        NewSignalBus(fetchers, cadences),
		evaluator:  NewTierEvaluator(bundle, cfg),
		dispatcher: NewDispatcher(32),
		cfg:        cfg,
		bundle:     bundle,
	}
}

// RegisterHandler adds a handler that receives tier transitions.
func (e *Engine) RegisterHandler(h func(TierTransition)) {
	e.dispatcher.Register(h)
}

// Start runs the engine until ctx is cancelled.
// It launches the signal bus, evaluator loop, and dispatcher drain concurrently.
func (e *Engine) Start(ctx context.Context) {
	// Start dispatcher drain in background.
	go e.dispatcher.Drain()

	// Start signal bus in background.
	go e.bus.Start(ctx)

	// Evaluator loop — runs every TickInterval.
	ticker := time.NewTicker(e.cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			e.dispatcher.Close()
			return
		case <-ticker.C:
			snap := e.bus.Snapshot()
			prevTier := e.evaluator.CurrentTier()
			newTier, changed := e.evaluator.Evaluate(snap)

			// Update signal value metrics on every tick. Tier metrics are recorded by
			// the enforcement layer, which knows the tier actually applied (a human
			// gate may be holding a transition the evaluator has already proposed).
			for signalID, val := range snap.Values {
				metrics.SignalValue.WithLabelValues(e.bundle.Service, signalID).Set(val)
			}

			if changed {
				e.dispatcher.Dispatch(TierTransition{
					From:    prevTier,
					To:      newTier,
					Signals: snap.Values,
				})
			}
		}
	}
}

// CurrentTier returns the active tier name (safe to call from any goroutine).
func (e *Engine) CurrentTier() string {
	return e.evaluator.CurrentTier()
}

// Bus returns the underlying SignalBus, e.g. for test injection.
func (e *Engine) Bus() *SignalBus {
	return e.bus
}

// LockTier forces the engine to hold tierName for duration, ignoring signal conditions.
// Used by the gate override and the control API /override endpoint.
func (e *Engine) LockTier(tierName string, duration time.Duration) error {
	return e.evaluator.LockTier(tierName, duration)
}

// Unlock releases any active tier lock.
func (e *Engine) Unlock() {
	e.evaluator.Unlock()
}

// LockState reports the active tier lock, if any.
func (e *Engine) LockState() (tier string, until time.Time, locked bool) {
	return e.evaluator.LockState()
}

// Bundle returns the compiled policy the engine is enforcing.
func (e *Engine) Bundle() *compiler.CompiledBundle {
	return e.bundle
}
