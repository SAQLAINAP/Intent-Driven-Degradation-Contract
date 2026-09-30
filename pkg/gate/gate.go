package gate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/audit"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/metrics"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
)

// Outcome represents the result of a human gate evaluation.
type Outcome string

const (
	OutcomeApproved   Outcome = "approved"
	OutcomeOverridden Outcome = "overridden"
	OutcomeEscalated  Outcome = "escalated"
	OutcomeTimedOut   Outcome = "timed_out"
	// OutcomeSuperseded means the gate was cancelled before anyone answered,
	// typically because signals moved to a different tier while it was open.
	OutcomeSuperseded Outcome = "superseded"
)

// GateResponse is sent by an operator via the HTTP callback API.
type GateResponse struct {
	Action      string // "approve", "override", "escalate"
	TargetTier  string // for override
	DurationMin int    // for override
	Message     string // for escalate
	Token       string
}

// Errors returned by Respond.
var (
	ErrUnauthorized    = errors.New("invalid auth token")
	ErrNoPendingGate   = errors.New("no gate currently pending")
	ErrAlreadyAnswered = errors.New("gate already answered")
)

// Decision is what Fire returns once the gate closes.
type Decision struct {
	Outcome    Outcome
	TargetTier string        // set for overrides
	Duration   time.Duration // override lock duration requested by the operator (0 = caller default)
	Message    string        // free-text note sent with an escalation
}

// Config holds gate configuration loaded from dg-engine.yaml.
type Config struct {
	SlackWebhookURL     string
	CallbackBaseURL     string
	AuthToken           string
	PagerDutyRoutingKey string
}

// Status is a point-in-time view of the gate, served by /gate/status.
type Status struct {
	State       string             `json:"state"` // "pending" or "idle"
	Tier        string             `json:"tier,omitempty"`
	FromTier    string             `json:"from_tier,omitempty"`
	OnTimeout   string             `json:"on_timeout,omitempty"`
	OpenedAt    *time.Time         `json:"opened_at,omitempty"`
	Deadline    *time.Time         `json:"deadline,omitempty"`
	Signals     map[string]float64 `json:"signals,omitempty"`
	LastOutcome string             `json:"last_outcome,omitempty"`
	LastTier    string             `json:"last_tier,omitempty"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// HumanGate implements the dead man's switch pattern for a single service.
// At most one gate is pending at a time; firing a new one supersedes routing
// of responses to the old one.
type HumanGate struct {
	cfg    Config
	logger *audit.Logger

	mu          sync.Mutex
	pending     *pendingGate
	lastOutcome Outcome
	lastTier    string
	updatedAt   time.Time
}

type pendingGate struct {
	tier      string
	fromTier  string
	onTimeout string
	openedAt  time.Time
	deadline  time.Time
	signals   map[string]float64
	respCh    chan GateResponse
}

// NewHumanGate creates a gate with the given config and audit logger.
func NewHumanGate(cfg Config, logger *audit.Logger) *HumanGate {
	return &HumanGate{
		cfg:       cfg,
		logger:    logger,
		updatedAt: time.Now(),
	}
}

// Fire notifies operators that svc wants to move from fromTier into tier and
// blocks until an operator responds, the spec's Wait elapses (OutcomeTimedOut),
// or ctx is cancelled (OutcomeSuperseded).
func (g *HumanGate) Fire(ctx context.Context, svc, fromTier, tier string, spec *policy.GateSpec, signals map[string]float64) Decision {
	if spec == nil {
		return Decision{Outcome: OutcomeTimedOut}
	}

	now := time.Now()
	p := &pendingGate{
		tier:      tier,
		fromTier:  fromTier,
		onTimeout: spec.OnTimeout,
		openedAt:  now,
		deadline:  now.Add(spec.Wait),
		signals:   signals,
		respCh:    make(chan GateResponse, 1),
	}
	g.mu.Lock()
	g.pending = p
	g.updatedAt = now
	g.mu.Unlock()

	// Send notifications to all configured backends (best-effort; don't block gate on failure).
	if g.cfg.SlackWebhookURL != "" {
		msg := buildSlackMessage(svc, tier, signals, spec.Wait, g.cfg.CallbackBaseURL)
		if err := sendSlack(g.cfg.SlackWebhookURL, msg, 3); err != nil {
			fmt.Printf("gate: slack notification failed: %v\n", err)
		}
	}
	if g.cfg.PagerDutyRoutingKey != "" {
		if err := sendPagerDuty(g.cfg.PagerDutyRoutingKey, svc, tier, signals); err != nil {
			fmt.Printf("gate: pagerduty notification failed: %v\n", err)
		}
	}

	g.logger.Log(audit.Event{
		Type:     audit.EventGateFired,
		Service:  svc,
		Tier:     tier,
		FromTier: fromTier,
		Signals:  signals,
	})

	// Dead man's switch: block until response or timeout.
	var d Decision
	timer := time.NewTimer(spec.Wait)
	defer timer.Stop()

	select {
	case resp := <-p.respCh:
		switch resp.Action {
		case "override":
			d = Decision{
				Outcome:    OutcomeOverridden,
				TargetTier: resp.TargetTier,
				Duration:   time.Duration(resp.DurationMin) * time.Minute,
			}
		case "escalate":
			d = Decision{Outcome: OutcomeEscalated, Message: resp.Message}
		default:
			d = Decision{Outcome: OutcomeApproved}
		}
	case <-timer.C:
		d = Decision{Outcome: OutcomeTimedOut}
	case <-ctx.Done():
		d = Decision{Outcome: OutcomeSuperseded}
	}

	g.mu.Lock()
	if g.pending == p { // a newer gate may already have replaced this one
		g.pending = nil
	}
	g.lastOutcome = d.Outcome
	g.lastTier = tier
	g.updatedAt = time.Now()
	g.mu.Unlock()

	ev := audit.Event{
		Type:        outcomeToEventType(d.Outcome),
		Service:     svc,
		Tier:        tier,
		FromTier:    fromTier,
		Signals:     signals,
		GateOutcome: string(d.Outcome),
	}
	if d.TargetTier != "" {
		target := d.TargetTier
		ev.OverrideTier = &target
	}
	g.logger.Log(ev)

	metrics.GateEventsTotal.WithLabelValues(svc, string(d.Outcome)).Inc()

	// Resolve the PagerDuty incident now that the gate has closed.
	if g.cfg.PagerDutyRoutingKey != "" {
		if err := resolvePagerDuty(g.cfg.PagerDutyRoutingKey, svc, tier); err != nil {
			fmt.Printf("gate: pagerduty resolve failed: %v\n", err)
		}
	}

	return d
}

// Respond delivers an operator response to the currently pending gate.
// Returns an error if no gate is currently pending or the token is invalid.
func (g *HumanGate) Respond(resp GateResponse) error {
	if g.cfg.AuthToken != "" && resp.Token != g.cfg.AuthToken {
		return ErrUnauthorized
	}
	g.mu.Lock()
	p := g.pending
	g.mu.Unlock()
	if p == nil {
		return ErrNoPendingGate
	}
	select {
	case p.respCh <- resp:
		return nil
	default:
		return ErrAlreadyAnswered
	}
}

// Status returns the current gate state.
func (g *HumanGate) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := Status{
		State:       "idle",
		LastOutcome: string(g.lastOutcome),
		LastTier:    g.lastTier,
		UpdatedAt:   g.updatedAt,
	}
	if p := g.pending; p != nil {
		opened, deadline := p.openedAt, p.deadline
		st.State = "pending"
		st.Tier = p.tier
		st.FromTier = p.fromTier
		st.OnTimeout = p.onTimeout
		if st.OnTimeout == "" {
			st.OnTimeout = "execute_policy"
		}
		st.OpenedAt = &opened
		st.Deadline = &deadline
		st.Signals = p.signals
	}
	return st
}

func outcomeToEventType(o Outcome) audit.EventType {
	switch o {
	case OutcomeApproved:
		return audit.EventGateApproved
	case OutcomeOverridden:
		return audit.EventGateOverridden
	case OutcomeEscalated:
		return audit.EventGateEscalated
	case OutcomeSuperseded:
		return audit.EventGateSuperseded
	default:
		return audit.EventGateTimedOut
	}
}
