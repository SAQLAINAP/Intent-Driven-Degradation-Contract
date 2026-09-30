// Package audit writes structured NDJSON event logs for all IDDC runtime events.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// EventType classifies an audit record.
type EventType string

const (
	EventTierTransition EventType = "tier_transition"
	EventGateFired      EventType = "gate_fired"
	EventGateApproved   EventType = "gate_approved"
	EventGateOverridden EventType = "gate_overridden"
	EventGateTimedOut   EventType = "gate_timed_out"
	EventGateEscalated  EventType = "gate_escalated"
	EventGateSuperseded EventType = "gate_superseded"
	EventTierOverride   EventType = "tier_override"
	EventQueueDrained   EventType = "queue_drained"
)

// Event is a single audit record.
type Event struct {
	Type         EventType          `json:"event"`
	Service      string             `json:"service"`
	Tier         string             `json:"tier,omitempty"`
	FromTier     string             `json:"from_tier,omitempty"`
	ToTier       string             `json:"to_tier,omitempty"`
	Signals      map[string]float64 `json:"signals,omitempty"`
	GateOutcome  string             `json:"gate_outcome,omitempty"`
	Reason       string             `json:"reason,omitempty"`
	Count        int                `json:"count,omitempty"`
	Operator     *string            `json:"operator"`
	OverrideTier *string            `json:"override_target"`
	Timestamp    time.Time          `json:"ts"`
}

// recentCap is how many events Recent can return.
const recentCap = 200

// Logger writes Events as NDJSON lines to an io.Writer and keeps the most
// recent events in memory for the control API / web console.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	recent []Event
}

// NewLogger creates a Logger writing to w.
func NewLogger(w io.Writer) *Logger {
	return &Logger{w: w}
}

// NewStdoutLogger returns a Logger that writes to stdout.
func NewStdoutLogger() *Logger {
	return NewLogger(os.Stdout)
}

// NewFileLogger opens (or creates) a file and returns a Logger writing to it.
func NewFileLogger(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening audit log: %w", err)
	}
	return NewLogger(f), nil
}

// Log serialises the event and writes it as one NDJSON line.
func (l *Logger) Log(e Event) {
	e.Timestamp = time.Now().UTC()
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Write(data)
	l.w.Write([]byte("\n"))
	l.recent = append(l.recent, e)
	if len(l.recent) > recentCap {
		l.recent = l.recent[len(l.recent)-recentCap:]
	}
}

// Recent returns up to n of the most recent events, newest first.
func (l *Logger) Recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.recent) {
		n = len(l.recent)
	}
	out := make([]Event, 0, n)
	for i := len(l.recent) - 1; i >= len(l.recent)-n; i-- {
		out = append(out, l.recent[i])
	}
	return out
}
