// Package sidecar runs the lightweight HTTP server embedded in the application pod.
// Applications query /flags to get the current feature flags for their active tier.
package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// TierState is the current state served to application code.
type TierState struct {
	Current string             `json:"current"`
	Since   time.Time          `json:"since"`
	Signals map[string]float64 `json:"signals"`
}

// FeatureFlags maps feature names to enabled/disabled for the current tier.
type FeatureFlags struct {
	Tier     string          `json:"tier"`
	Features map[string]bool `json:"features"`
}

// Server is the sidecar HTTP server.
type Server struct {
	mu        sync.RWMutex
	state     TierState
	features  map[string]bool
	authToken string
	addr      string
	mux       *http.ServeMux
	server    *http.Server
}

// NewServer creates a sidecar server on the given address.
func NewServer(addr, authToken string) *Server {
	s := &Server{
		addr:      addr,
		authToken: authToken,
		features:  allFeaturesEnabled(),
		state: TierState{
			Current: "nominal",
			Since:   time.Now(),
		},
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/flags", s.handleFlags)
	s.mux.HandleFunc("/tier", s.handleTier)
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/override", s.handleOverride)
	s.server = &http.Server{Addr: addr, Handler: s.mux}
	return s
}

// ServeHTTP implements http.Handler, enabling use with httptest.NewRecorder in tests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Start begins listening. Returns when ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(shutCtx)
	}()
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("sidecar server: %w", err)
	}
	return nil
}

// SetTier updates the active tier and recalculates feature flags.
func (s *Server) SetTier(tier string, signals map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = TierState{Current: tier, Since: time.Now(), Signals: signals}
	s.features = featuresForTier(tier)
}

// FallbackActive reports whether the current tier requires the static fallback
// page (i.e. survival tier — all features disabled, return 503).
func (s *Server) FallbackActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Current == "survival"
}

// Tier returns the current tier name.
func (s *Server) Tier() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Current
}

func (s *Server) handleFlags(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, FeatureFlags{Tier: s.state.Current, Features: s.features})
}

func (s *Server) handleTier(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, s.state)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

type overrideRequest struct {
	Tier  string `json:"tier"`
	Token string `json:"token"`
}

func (s *Server) handleOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req overrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if s.authToken != "" && req.Token != s.authToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.SetTier(req.Tier, nil)
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func allFeaturesEnabled() map[string]bool {
	return map[string]bool{
		"recommendations": true,
		"analytics":       true,
		"uploads":         true,
	}
}

// featuresForTier returns the feature flag map for a given tier name.
// This implements the behavior table from the spec.
func featuresForTier(tier string) map[string]bool {
	switch tier {
	case "nominal":
		return map[string]bool{"recommendations": true, "analytics": true, "uploads": true}
	case "warm":
		return map[string]bool{"recommendations": false, "analytics": false, "uploads": true}
	case "hot":
		return map[string]bool{"recommendations": false, "analytics": false, "uploads": false}
	case "critical", "survival":
		return map[string]bool{"recommendations": false, "analytics": false, "uploads": false}
	default:
		return allFeaturesEnabled()
	}
}
