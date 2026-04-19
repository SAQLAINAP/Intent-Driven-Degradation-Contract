package gate_test

import (
	"context"
	"testing"
	"time"

	"iddc/pkg/audit"
	"iddc/pkg/gate"
	"iddc/pkg/policy"
)

func newGate() *gate.HumanGate {
	return gate.NewHumanGate(gate.Config{}, audit.NewStdoutLogger())
}

func spec(wait time.Duration, onTimeout string) *policy.GateSpec {
	return &policy.GateSpec{
		Wait:      wait,
		OnTimeout: onTimeout,
	}
}

// TC-G-01: Gate times out when no response arrives within Wait.
func TestGate_Timeout(t *testing.T) {
	g := newGate()
	outcome, tier := g.Fire(context.Background(), "svc", spec(50*time.Millisecond, "execute_policy"), nil)
	if outcome != gate.OutcomeTimedOut {
		t.Errorf("outcome = %q, want timed_out", outcome)
	}
	if tier != "" {
		t.Errorf("overrideTier = %q, want empty", tier)
	}
}

// TC-G-02: Respond(approve) unblocks Fire and returns approved.
func TestGate_Approve(t *testing.T) {
	g := newGate()
	done := make(chan struct {
		o gate.Outcome
		t string
	}, 1)

	go func() {
		o, t := g.Fire(context.Background(), "svc", spec(5*time.Second, "execute_policy"), nil)
		done <- struct {
			o gate.Outcome
			t string
		}{o, t}
	}()

	time.Sleep(10 * time.Millisecond) // let Fire register the pending gate
	if err := g.Respond(gate.GateResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	result := <-done
	if result.o != gate.OutcomeApproved {
		t.Errorf("outcome = %q, want approved", result.o)
	}
}

// TC-G-03: Respond(override) sets overrideTier correctly.
func TestGate_Override(t *testing.T) {
	g := newGate()
	done := make(chan struct {
		o gate.Outcome
		t string
	}, 1)

	go func() {
		o, t := g.Fire(context.Background(), "svc", spec(5*time.Second, "hold"), nil)
		done <- struct {
			o gate.Outcome
			t string
		}{o, t}
	}()

	time.Sleep(10 * time.Millisecond)
	if err := g.Respond(gate.GateResponse{Action: "override", TargetTier: "nominal"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	result := <-done
	if result.o != gate.OutcomeOverridden {
		t.Errorf("outcome = %q, want overridden", result.o)
	}
	if result.t != "nominal" {
		t.Errorf("overrideTier = %q, want nominal", result.t)
	}
}

// TC-G-04: Respond when no gate is pending returns an error.
func TestGate_RespondNoPending(t *testing.T) {
	g := newGate()
	if err := g.Respond(gate.GateResponse{Action: "approve"}); err == nil {
		t.Error("expected error when no gate is pending")
	}
}

// TC-G-05: nil spec returns timed_out immediately.
func TestGate_NilSpec(t *testing.T) {
	g := newGate()
	outcome, _ := g.Fire(context.Background(), "svc", nil, nil)
	if outcome != gate.OutcomeTimedOut {
		t.Errorf("outcome = %q, want timed_out for nil spec", outcome)
	}
}

// TC-G-06: ctx cancellation unblocks Fire.
func TestGate_ContextCancel(t *testing.T) {
	g := newGate()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan gate.Outcome, 1)
	go func() {
		o, _ := g.Fire(ctx, "svc", spec(10*time.Second, "execute_policy"), nil)
		done <- o
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case o := <-done:
		if o != gate.OutcomeTimedOut {
			t.Errorf("outcome after ctx cancel = %q, want timed_out", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fire did not return after context cancellation")
	}
}

// TC-G-07: auth token validation rejects wrong token.
func TestGate_AuthToken(t *testing.T) {
	g := gate.NewHumanGate(gate.Config{AuthToken: "secret"}, audit.NewStdoutLogger())
	done := make(chan struct{}, 1)
	go func() {
		g.Fire(context.Background(), "svc", spec(2*time.Second, "execute_policy"), nil)
		done <- struct{}{}
	}()
	time.Sleep(10 * time.Millisecond)

	if err := g.Respond(gate.GateResponse{Action: "approve", Token: "wrong"}); err == nil {
		t.Error("expected auth error for wrong token")
	}
	if err := g.Respond(gate.GateResponse{Action: "approve", Token: "secret"}); err != nil {
		t.Errorf("Respond with correct token: %v", err)
	}
	<-done
}
