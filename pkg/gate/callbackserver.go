package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CallbackServer listens for operator responses sent by Slack button clicks.
// It routes incoming POSTs to the HumanGate's Respond() method.
type CallbackServer struct {
	gate    *HumanGate
	addr    string
	handler http.Handler
	server  *http.Server
}

// NewCallbackServer creates a server on addr (e.g. ":9292") backed by gate.
func NewCallbackServer(addr string, gate *HumanGate) *CallbackServer {
	cs := &CallbackServer{
		gate: gate,
		addr: addr,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/gate/approve", cs.handleApprove)
	mux.HandleFunc("/gate/override", cs.handleOverride)
	mux.HandleFunc("/gate/escalate", cs.handleEscalate)
	mux.HandleFunc("/gate/status", cs.handleStatus)

	cs.handler = mux
	cs.server = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return cs
}

// Handler exposes the /gate/* routes so they can also be mounted on another
// server (the engine's control API mounts them for the web console).
func (cs *CallbackServer) Handler() http.Handler {
	return cs.handler
}

// Start begins listening. Returns when ctx is cancelled.
func (cs *CallbackServer) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		// ctx is already cancelled here; keep its values but give shutdown a fresh deadline.
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = cs.server.Shutdown(shutCtx) // best effort: the process is exiting
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
	if !decodeOptional(w, r, &req) {
		return
	}

	if err := cs.gate.Respond(GateResponse{Action: "approve", Token: req.Token}); err != nil {
		respondError(w, err)
		return
	}
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
	if !decodeOptional(w, r, &req) {
		return
	}
	if req.TargetTier == "" {
		http.Error(w, "target_tier is required", http.StatusBadRequest)
		return
	}

	if err := cs.gate.Respond(GateResponse{
		Action:      "override",
		TargetTier:  req.TargetTier,
		DurationMin: req.DurationMin,
		Token:       req.Token,
	}); err != nil {
		respondError(w, err)
		return
	}
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
	if !decodeOptional(w, r, &req) {
		return
	}

	if err := cs.gate.Respond(GateResponse{
		Action:  "escalate",
		Message: req.Message,
		Token:   req.Token,
	}); err != nil {
		respondError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "escalated"})
}

func (cs *CallbackServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, cs.gate.Status())
}

// respondError maps Respond errors to HTTP status codes.
func respondError(w http.ResponseWriter, err error) {
	status := http.StatusConflict // no gate pending / already answered
	if errors.Is(err, ErrUnauthorized) {
		status = http.StatusUnauthorized
	}
	http.Error(w, err.Error(), status)
}

// decodeOptional decodes a JSON body if one is present. An empty body is
// allowed (all fields optional); malformed JSON is rejected with 400.
func decodeOptional(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	if err != nil && err != io.EOF {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
