// Package runtime implements the signal bus, tier evaluator, dispatcher, and engine.
package runtime

import (
	"context"
	"sync"
	"time"
)

// SignalSnapshot holds the latest sampled value for every declared signal.
// All access must go through SignalBus to guarantee thread safety.
type SignalSnapshot struct {
	Values    map[string]float64
	Stale     map[string]bool // true if value hasn't refreshed within StaleTimeout
	SampledAt time.Time
}

// SignalFetcher is a function that returns the current value for a single signal.
// Each signal source (Prometheus, /proc, injected test value) implements this signature.
type SignalFetcher func(ctx context.Context) (float64, error)

// signalState tracks per-signal metadata inside the bus.
type signalState struct {
	lastValue    float64
	lastFetchAt  time.Time
	stale        bool
}

// SignalBus runs one goroutine per signal, writing into a shared SignalSnapshot
// protected by a sync.RWMutex.
type SignalBus struct {
	mu            sync.RWMutex
	snapshot      SignalSnapshot
	fetchers      map[string]SignalFetcher
	pollCadence   map[string]time.Duration
	staleTimeout  time.Duration
	defaultCadence time.Duration
}

const defaultPollCadence = 2 * time.Second
const defaultStaleTimeout = 30 * time.Second

// NewSignalBus creates a bus for the given set of (signalID → fetcher) pairs.
func NewSignalBus(fetchers map[string]SignalFetcher, cadences map[string]time.Duration) *SignalBus {
	if cadences == nil {
		cadences = map[string]time.Duration{}
	}
	return &SignalBus{
		snapshot: SignalSnapshot{
			Values: make(map[string]float64),
			Stale:  make(map[string]bool),
		},
		fetchers:       fetchers,
		pollCadence:    cadences,
		staleTimeout:   defaultStaleTimeout,
		defaultCadence: defaultPollCadence,
	}
}

// Start launches one polling goroutine per signal. It returns when ctx is cancelled.
func (b *SignalBus) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for id, fetcher := range b.fetchers {
		wg.Add(1)
		go func(signalID string, fn SignalFetcher) {
			defer wg.Done()
			cadence := b.pollCadence[signalID]
			if cadence == 0 {
				cadence = b.defaultCadence
			}
			ticker := time.NewTicker(cadence)
			defer ticker.Stop()

			state := &signalState{}

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					val, err := fn(ctx)
					now := time.Now()
					b.mu.Lock()
					if err == nil {
						b.snapshot.Values[signalID] = val
						b.snapshot.Stale[signalID] = false
						state.lastValue = val
						state.lastFetchAt = now
						state.stale = false
					} else {
						// Keep last known value, mark stale if timeout exceeded.
						if !state.lastFetchAt.IsZero() && now.Sub(state.lastFetchAt) > b.staleTimeout {
							b.snapshot.Stale[signalID] = true
							state.stale = true
						}
					}
					b.snapshot.SampledAt = now
					b.mu.Unlock()
				}
			}
		}(id, fetcher)
	}
	wg.Wait()
}

// Snapshot returns a point-in-time copy of the current signal values.
// The caller receives a copy; no locking is required after this call.
func (b *SignalBus) Snapshot() SignalSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	snap := SignalSnapshot{
		Values:    make(map[string]float64, len(b.snapshot.Values)),
		Stale:     make(map[string]bool, len(b.snapshot.Stale)),
		SampledAt: b.snapshot.SampledAt,
	}
	for k, v := range b.snapshot.Values {
		snap.Values[k] = v
	}
	for k, v := range b.snapshot.Stale {
		snap.Stale[k] = v
	}
	return snap
}

// Inject directly sets a signal value — used by tests and the `dg simulate` command.
func (b *SignalBus) Inject(signalID string, value float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snapshot.Values[signalID] = value
	b.snapshot.Stale[signalID] = false
	b.snapshot.SampledAt = time.Now()
}
