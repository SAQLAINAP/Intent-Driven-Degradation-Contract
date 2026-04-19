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
)

// Event is a single audit record.
type Event struct {
	Type           EventType          `json:"event"`
	Service        string             `json:"service"`
	Tier           string             `json:"tier,omitempty"`
	FromTier       string             `json:"from_tier,omitempty"`
	ToTier         string             `json:"to_tier,omitempty"`
	Signals        map[string]float64 `json:"signals,omitempty"`
	GateOutcome    string             `json:"gate_outcome,omitempty"`
	Operator       *string            `json:"operator"`
	OverrideTier   *string            `json:"override_target"`
	Timestamp      time.Time          `json:"ts"`
}

// Logger writes Events as NDJSON lines to an io.Writer.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
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
}
