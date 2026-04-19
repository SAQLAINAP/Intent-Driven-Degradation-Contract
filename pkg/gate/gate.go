package gate

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"iddc/pkg/audit"
	"iddc/pkg/metrics"
	"iddc/pkg/policy"
)

// Outcome represents the result of a human gate evaluation.
type Outcome string

const (
	OutcomeApproved   Outcome = "approved"
	OutcomeOverridden Outcome = "overridden"
	OutcomeEscalated  Outcome = "escalated"
	OutcomeTimedOut   Outcome = "timed_out"
)

// GateResponse is sent by an operator via the HTTP callback API.
type GateResponse struct {
	Action      string // "approve", "override", "escalate"
	TargetTier  string // for override
	DurationMin int    // for override
	Message     string // for escalate
	Token       string
}

// Config holds gate configuration loaded from dg-engine.yaml.
type Config struct {
	SlackWebhookURL      string
	CallbackBaseURL      string
	AuthToken            string
	PagerDutyRoutingKey  string
}

// HumanGate implements the dead man's switch pattern for a single service.
type HumanGate struct {
	cfg      Config
	logger   *audit.Logger
	mu       sync.Mutex
	pending  *pendingGate
	server   *http.Server
}

type pendingGate struct {
	tier    string
	signals map[string]float64
	respCh  chan GateResponse
}

// NewHumanGate creates a gate with the given config and audit logger.
func NewHumanGate(cfg Config, logger *audit.Logger) *HumanGate {
	return &HumanGate{
		cfg:    cfg,
		logger: logger,
	}
}

// Fire sends a Slack notification and blocks until the operator responds
// or the gate spec's Wait timeout expires.
// Returns the final outcome and, for overrides, the target tier.
func (g *HumanGate) Fire(ctx context.Context, svc string, spec *policy.GateSpec, signals map[string]float64) (Outcome, string) {
	if spec == nil {
		return OutcomeTimedOut, ""
	}

	// Register this gate as pending so the callback server can route responses.
	respCh := make(chan GateResponse, 1)
	g.mu.Lock()
	g.pending = &pendingGate{
		tier:    spec.OnTimeout,
		signals: signals,
		respCh:  respCh,
	}
	g.mu.Unlock()

	// Send notifications to all configured backends (best-effort; don't block gate on failure).
	if g.cfg.SlackWebhookURL != "" {
		msg := buildSlackMessage(svc, "critical", signals, spec.Wait, g.cfg.CallbackBaseURL)
		if err := sendSlack(g.cfg.SlackWebhookURL, msg, 3); err != nil {
			fmt.Printf("gate: slack notification failed: %v\n", err)
		}
	}
	if g.cfg.PagerDutyRoutingKey != "" {
		if err := sendPagerDuty(g.cfg.PagerDutyRoutingKey, svc, "critical", signals); err != nil {
			fmt.Printf("gate: pagerduty notification failed: %v\n", err)
		}
	}

	g.logger.Log(audit.Event{
		Type:    audit.EventGateFired,
		Service: svc,
		Tier:    "critical",
		Signals: signals,
	})

	// Dead man's switch: block until response or timeout.
	var outcome Outcome
	var overrideTier string

	select {
	case resp := <-respCh:
		switch resp.Action {
		case "approve":
			outcome = OutcomeApproved
		case "override":
			outcome = OutcomeOverridden
			overrideTier = resp.TargetTier
		case "escalate":
			outcome = OutcomeEscalated
			overrideTier = "survival"
		default:
			outcome = OutcomeApproved
		}
	case <-time.After(spec.Wait):
		outcome = OutcomeTimedOut
	case <-ctx.Done():
		outcome = OutcomeTimedOut
	}

	g.mu.Lock()
	g.pending = nil
	g.mu.Unlock()

	g.logger.Log(audit.Event{
		Type:    outcomeToEventType(outcome),
		Service: svc,
		Tier:    "critical",
		Signals: signals,
	})

	metrics.GateEventsTotal.WithLabelValues(svc, string(outcome)).Inc()

	// Resolve the PagerDuty incident now that the gate has closed.
	if g.cfg.PagerDutyRoutingKey != "" {
		if err := resolvePagerDuty(g.cfg.PagerDutyRoutingKey, svc, "critical"); err != nil {
			fmt.Printf("gate: pagerduty resolve failed: %v\n", err)
		}
	}

	return outcome, overrideTier
}

// Respond delivers an operator response to the currently pending gate.
// Returns an error if no gate is currently pending or the token is invalid.
func (g *HumanGate) Respond(resp GateResponse) error {
	if g.cfg.AuthToken != "" && resp.Token != g.cfg.AuthToken {
		return fmt.Errorf("invalid auth token")
	}
	g.mu.Lock()
	p := g.pending
	g.mu.Unlock()
	if p == nil {
		return fmt.Errorf("no gate currently pending")
	}
	select {
	case p.respCh <- resp:
		return nil
	default:
		return fmt.Errorf("gate response channel full")
	}
}

func outcomeToEventType(o Outcome) audit.EventType {
	switch o {
	case OutcomeApproved:
		return audit.EventGateApproved
	case OutcomeOverridden:
		return audit.EventGateOverridden
	default:
		return audit.EventGateTimedOut
	}
}
