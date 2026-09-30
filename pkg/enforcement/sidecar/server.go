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

	"iddc/pkg/compiler"
)

// TierState is the current state served to application code.
type TierState struct {
	Current string             `json:"current"`
	Mode    string             `json:"mode,omitempty"`
	Since   time.Time          `json:"since"`
	Signals map[string]float64 `json:"signals"`
}

// FeatureFlags maps feature names to enabled/disabled for the current tier.
// Mode is the tier's behavior mode (full_service, read_only, static_fallback)
// so applications can react to the mode rather than hard-coding tier names.
type FeatureFlags struct {
	Tier     string          `json:"tier"`
	Mode     string          `json:"mode,omitempty"`
	Features map[string]bool `json:"features"`
}

// TierBehavior is the per-tier feature state the sidecar serves.
type TierBehavior struct {
	Mode     string
	Features map[string]bool
}

// Policy is the feature table derived from a compiled bundle.
type Policy struct {
	InitialTier string
	Tiers       map[string]TierBehavior
}

// PolicyFromBundle derives the feature table from a compiled policy:
//
//   - the feature universe is every name listed in any tier's disable: list;
//   - a full_service tier enables every feature except the ones it disables;
//   - read_only and static_fallback tiers disable every feature.
func PolicyFromBundle(b *compiler.CompiledBundle) Policy {
	universe := map[string]bool{}
	for _, t := range b.Tiers {
		for _, f := range t.Behavior.Disable {
			universe[f] = true
		}
	}
	p := Policy{Tiers: make(map[string]TierBehavior, len(b.Tiers))}
	if len(b.Tiers) > 0 {
		p.InitialTier = b.Tiers[0].Name
	}
	for _, t := range b.Tiers {
		mode := t.Behavior.Mode
		if mode == "" {
			mode = "full_service"
		}
		features := make(map[string]bool, len(universe))
		for f := range universe {
			features[f] = mode == "full_service"
		}
		for _, f := range t.Behavior.Disable {
			features[f] = false
		}
		p.Tiers[t.Name] = TierBehavior{Mode: mode, Features: features}
	}
	return p
}

// Server is the sidecar HTTP server.
type Server struct {
	mu        sync.RWMutex
	state     TierState
	features  map[string]bool
	policy    *Policy
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

// UsePolicy switches the server from the built-in demo feature table to one
// derived from a compiled policy, and resets the state to the policy's first tier.
func (s *Server) UsePolicy(p Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = &p
	s.state = TierState{Current: p.InitialTier, Mode: p.Tiers[p.InitialTier].Mode, Since: time.Now()}
	s.features = s.featuresForTierLocked(p.InitialTier)
}

// KnownTier reports whether tier exists in the active policy (always true
// when no policy has been loaded).
func (s *Server) KnownTier(tier string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.policy == nil {
		return true
	}
	_, ok := s.policy.Tiers[tier]
	return ok
}

// Features returns a copy of the current feature flags.
func (s *Server) Features() FeatureFlags {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.flagsLocked()
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
	s.state = TierState{Current: tier, Mode: s.modeLocked(tier), Since: time.Now(), Signals: signals}
	s.features = s.featuresForTierLocked(tier)
}

// FallbackActive reports whether the current tier requires the static fallback
// page (static_fallback mode — all features disabled, return 503).
func (s *Server) FallbackActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.policy != nil {
		return s.state.Mode == "static_fallback"
	}
	return s.state.Current == "survival"
}

func (s *Server) modeLocked(tier string) string {
	if s.policy != nil {
		return s.policy.Tiers[tier].Mode
	}
	return ""
}

func (s *Server) featuresForTierLocked(tier string) map[string]bool {
	if s.policy == nil {
		return featuresForTier(tier)
	}
	src := s.policy.Tiers[tier].Features
	out := make(map[string]bool, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (s *Server) flagsLocked() FeatureFlags {
	features := make(map[string]bool, len(s.features))
	for k, v := range s.features {
		features[k] = v
	}
	return FeatureFlags{Tier: s.state.Current, Mode: s.state.Mode, Features: features}
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
	writeJSON(w, s.flagsLocked())
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
	if req.Tier == "" || !s.KnownTier(req.Tier) {
		http.Error(w, fmt.Sprintf("unknown tier %q", req.Tier), http.StatusBadRequest)
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

// featuresForTier is the built-in demo table used when no policy is loaded
// (see UsePolicy). It mirrors config/example-degradation.yaml.
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
