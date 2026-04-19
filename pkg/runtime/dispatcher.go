package runtime

// TierTransition describes a confirmed tier change event sent to enforcement.
type TierTransition struct {
	From    string
	To      string
	Signals map[string]float64
}

// Dispatcher fans out tier transition events to registered handlers.
// It fires only on confirmed transitions — not on every evaluator tick.
type Dispatcher struct {
	ch       chan TierTransition
	handlers []func(TierTransition)
}

// NewDispatcher creates a Dispatcher with a buffered internal channel.
func NewDispatcher(bufSize int) *Dispatcher {
	d := &Dispatcher{
		ch: make(chan TierTransition, bufSize),
	}
	return d
}

// Register adds a handler function that will be called for every transition.
// Handlers are called synchronously inside the Dispatcher's drain loop.
func (d *Dispatcher) Register(h func(TierTransition)) {
	d.handlers = append(d.handlers, h)
}

// Dispatch enqueues a transition event. Non-blocking; drops if buffer full
// (the engine logs this case).
func (d *Dispatcher) Dispatch(t TierTransition) {
	select {
	case d.ch <- t:
	default:
		// Buffer full — log would go here in production.
	}
}

// Drain reads from the internal channel and calls all handlers until the
// channel is closed. Call this in a dedicated goroutine.
func (d *Dispatcher) Drain() {
	for t := range d.ch {
		for _, h := range d.handlers {
			h(t)
		}
	}
}

// Close shuts the dispatcher down after all pending events are drained.
func (d *Dispatcher) Close() {
	close(d.ch)
}
