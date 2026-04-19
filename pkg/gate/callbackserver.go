package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// CallbackServer listens for operator responses sent by Slack button clicks.
// It routes incoming POSTs to the HumanGate's Respond() method.
type CallbackServer struct {
	gate   *HumanGate
	addr   string
	status callbackStatus
	server *http.Server
}

type callbackStatus struct {
	State     string    `json:"state"` // waiting / approved / overridden / timed_out
	UpdatedAt time.Time `json:"updated_at"`
}

// NewCallbackServer creates a server on addr (e.g. ":9292") backed by gate.
func NewCallbackServer(addr string, gate *HumanGate) *CallbackServer {
	cs := &CallbackServer{
		gate:   gate,
		addr:   addr,
		status: callbackStatus{State: "waiting", UpdatedAt: time.Now()},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/gate/approve",  cs.handleApprove)
	mux.HandleFunc("/gate/override", cs.handleOverride)
	mux.HandleFunc("/gate/escalate", cs.handleEscalate)
	mux.HandleFunc("/gate/status",   cs.handleStatus)

	cs.server = &http.Server{Addr: addr, Handler: mux}
	return cs
}

// Start begins listening. Returns when ctx is cancelled.
func (cs *CallbackServer) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cs.server.Shutdown(shutCtx)
	}()
	if err := cs.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("gate callback server: %w", err)
	}
	return nil
}

// ─── handlers ────────────────────────────────────────────────────────────────

type approveRequest struct {
	Tier  string `json:"tier"`
	Token string `json:"token"`
}

func (cs *CallbackServer) handleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req approveRequest
	json.NewDecoder(r.Body).Decode(&req)

	if err := cs.gate.Respond(GateResponse{Action: "approve", Token: req.Token}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	cs.status = callbackStatus{State: "approved", UpdatedAt: time.Now()}
	writeJSON(w, map[string]string{"status": "approved"})
}

type overrideRequest struct {
	TargetTier  string `json:"target_tier"`
	DurationMin int    `json:"duration_min"`
	Token       string `json:"token"`
}

func (cs *CallbackServer) handleOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req overrideRequest
	json.NewDecoder(r.Body).Decode(&req)

	if err := cs.gate.Respond(GateResponse{
		Action:      "override",
		TargetTier:  req.TargetTier,
		DurationMin: req.DurationMin,
		Token:       req.Token,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	cs.status = callbackStatus{State: "overridden", UpdatedAt: time.Now()}
	writeJSON(w, map[string]string{"status": "overridden", "target_tier": req.TargetTier})
}

type escalateRequest struct {
	Message string `json:"message"`
	Token   string `json:"token"`
}

func (cs *CallbackServer) handleEscalate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req escalateRequest
	json.NewDecoder(r.Body).Decode(&req)

	if err := cs.gate.Respond(GateResponse{
		Action:  "escalate",
		Message: req.Message,
		Token:   req.Token,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	cs.status = callbackStatus{State: "escalated", UpdatedAt: time.Now()}
	writeJSON(w, map[string]string{"status": "escalated"})
}

func (cs *CallbackServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, cs.status)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
