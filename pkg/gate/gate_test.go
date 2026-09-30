package gate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/audit"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/gate"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
)

func newGate() *gate.HumanGate {
	return gate.NewHumanGate(gate.Config{}, audit.NewLogger(&bytes.Buffer{}))
}

func spec(wait time.Duration, onTimeout string) *policy.GateSpec {
	return &policy.GateSpec{
		Wait:      wait,
		OnTimeout: onTimeout,
	}
}

// fireAsync starts Fire in a goroutine and waits until the gate is pending.
func fireAsync(t *testing.T, g *gate.HumanGate, ctx context.Context, tier string, s *policy.GateSpec) <-chan gate.Decision {
	t.Helper()
	done := make(chan gate.Decision, 1)
	go func() { done <- g.Fire(ctx, "svc", "hot", tier, s, nil) }()
	deadline := time.Now().Add(time.Second)
	for g.Status().State != "pending" || g.Status().Tier != tier {
		if time.Now().After(deadline) {
			t.Fatal("gate never became pending")
		}
		time.Sleep(time.Millisecond)
	}
	return done
}

// TC-G-01: Gate times out when no response arrives within Wait.
func TestGate_Timeout(t *testing.T) {
	g := newGate()
	d := g.Fire(context.Background(), "svc", "hot", "critical", spec(50*time.Millisecond, "execute_policy"), nil)
	if d.Outcome != gate.OutcomeTimedOut {
		t.Errorf("outcome = %q, want timed_out", d.Outcome)
	}
	if d.TargetTier != "" {
		t.Errorf("TargetTier = %q, want empty", d.TargetTier)
	}
}

// TC-G-02: Respond(approve) unblocks Fire and returns approved.
func TestGate_Approve(t *testing.T) {
	g := newGate()
	done := fireAsync(t, g, context.Background(), "critical", spec(5*time.Second, "execute_policy"))
	if err := g.Respond(gate.GateResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if d := <-done; d.Outcome != gate.OutcomeApproved {
		t.Errorf("outcome = %q, want approved", d.Outcome)
	}
}

// TC-G-03: Respond(override) returns the target tier and requested duration.
func TestGate_Override(t *testing.T) {
	g := newGate()
	done := fireAsync(t, g, context.Background(), "critical", spec(5*time.Second, "hold"))
	if err := g.Respond(gate.GateResponse{Action: "override", TargetTier: "nominal", DurationMin: 3}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	d := <-done
	if d.Outcome != gate.OutcomeOverridden {
		t.Errorf("outcome = %q, want overridden", d.Outcome)
	}
	if d.TargetTier != "nominal" {
		t.Errorf("TargetTier = %q, want nominal", d.TargetTier)
	}
	if d.Duration != 3*time.Minute {
		t.Errorf("Duration = %s, want 3m", d.Duration)
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
	if d := g.Fire(context.Background(), "svc", "hot", "critical", nil, nil); d.Outcome != gate.OutcomeTimedOut {
		t.Errorf("outcome = %q, want timed_out for nil spec", d.Outcome)
	}
}

// TC-G-06: ctx cancellation unblocks Fire and reports the gate as superseded.
func TestGate_ContextCancel(t *testing.T) {
	g := newGate()
	ctx, cancel := context.WithCancel(context.Background())
	done := fireAsync(t, g, ctx, "critical", spec(10*time.Second, "execute_policy"))
	cancel()

	select {
	case d := <-done:
		if d.Outcome != gate.OutcomeSuperseded {
			t.Errorf("outcome after ctx cancel = %q, want superseded", d.Outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fire did not return after context cancellation")
	}
}

// TC-G-07: auth token validation rejects wrong token.
func TestGate_AuthToken(t *testing.T) {
	g := gate.NewHumanGate(gate.Config{AuthToken: "secret"}, audit.NewLogger(&bytes.Buffer{}))
	done := fireAsync(t, g, context.Background(), "critical", spec(2*time.Second, "execute_policy"))

	if err := g.Respond(gate.GateResponse{Action: "approve", Token: "wrong"}); err == nil {
		t.Error("expected auth error for wrong token")
	}
	if err := g.Respond(gate.GateResponse{Action: "approve", Token: "secret"}); err != nil {
		t.Errorf("Respond with correct token: %v", err)
	}
	<-done
}

// Regression: audit events and status carry the real tier, not a hard-coded "critical".
func TestGate_UsesActualTierName(t *testing.T) {
	var buf bytes.Buffer
	g := gate.NewHumanGate(gate.Config{}, audit.NewLogger(&buf))
	done := fireAsync(t, g, context.Background(), "survival", spec(5*time.Second, "escalate"))

	st := g.Status()
	if st.Tier != "survival" || st.FromTier != "hot" || st.OnTimeout != "escalate" || st.Deadline == nil {
		t.Errorf("pending status = %+v", st)
	}
	g.Respond(gate.GateResponse{Action: "escalate"})
	if d := <-done; d.Outcome != gate.OutcomeEscalated {
		t.Errorf("outcome = %q, want escalated", d.Outcome)
	}
	if strings.Contains(buf.String(), `"critical"`) {
		t.Errorf("audit log mentions critical for a survival gate:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"event":"gate_escalated"`) {
		t.Errorf("expected gate_escalated audit event:\n%s", buf.String())
	}
	if st := g.Status(); st.State != "idle" || st.LastOutcome != "escalated" || st.LastTier != "survival" {
		t.Errorf("status after close = %+v", st)
	}
}

// Regression: a superseded gate finishing late must not clear a newer pending gate.
func TestGate_OldGateDoesNotClobberNewer(t *testing.T) {
	g := newGate()
	ctx1, cancel1 := context.WithCancel(context.Background())
	old := fireAsync(t, g, ctx1, "critical", spec(10*time.Second, "execute_policy"))
	newer := fireAsync(t, g, context.Background(), "survival", spec(10*time.Second, "execute_policy"))

	cancel1()
	<-old
	if st := g.Status(); st.State != "pending" || st.Tier != "survival" {
		t.Fatalf("newer gate lost after old gate closed: %+v", st)
	}
	if err := g.Respond(gate.GateResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond to newer gate: %v", err)
	}
	if d := <-newer; d.Outcome != gate.OutcomeApproved {
		t.Errorf("newer outcome = %q", d.Outcome)
	}
}

// Callback server: /gate/status reports idle vs pending; override requires a target.
func TestCallbackServer_StatusAndValidation(t *testing.T) {
	g := newGate()
	h := gate.NewCallbackServer(":0", g).Handler()

	get := func() gate.Status {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gate/status", nil))
		var st gate.Status
		json.NewDecoder(rec.Body).Decode(&st)
		return st
	}
	if st := get(); st.State != "idle" {
		t.Errorf("state with nothing pending = %q, want idle", st.State)
	}

	done := fireAsync(t, g, context.Background(), "critical", spec(5*time.Second, "execute_policy"))
	if st := get(); st.State != "pending" || st.Tier != "critical" {
		t.Errorf("status while pending = %+v", st)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/gate/override", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("override without target_tier = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/gate/approve", strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("approve with malformed JSON = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/gate/approve", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("approve with empty body = %d, want 200", rec.Code)
	}
	<-done
}

// The callback API distinguishes a bad token (401) from "nothing to answer" (409).
func TestCallbackServer_StatusCodes(t *testing.T) {
	g := gate.NewHumanGate(gate.Config{AuthToken: "secret"}, audit.NewLogger(&bytes.Buffer{}))
	h := gate.NewCallbackServer(":0", g).Handler()
	post := func(body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/gate/approve", strings.NewReader(body)))
		return rec.Code
	}
	if code := post(`{}`); code != http.StatusUnauthorized {
		t.Errorf("missing token = %d, want 401", code)
	}
	if code := post(`{"token":"secret"}`); code != http.StatusConflict {
		t.Errorf("valid token, nothing pending = %d, want 409", code)
	}
	done := fireAsync(t, g, context.Background(), "critical", spec(5*time.Second, "execute_policy"))
	if code := post(`{"token":"wrong"}`); code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", code)
	}
	if code := post(`{"token":"secret"}`); code != http.StatusOK {
		t.Errorf("valid token = %d, want 200", code)
	}
	<-done
}
