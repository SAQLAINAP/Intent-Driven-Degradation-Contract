package sidecar_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"iddc/pkg/compiler"
	"iddc/pkg/enforcement/sidecar"
	"iddc/pkg/policy"
)

func newSrv() *sidecar.Server {
	return sidecar.NewServer(":0", "")
}

func getFlags(t *testing.T, srv *sidecar.Server) sidecar.FeatureFlags {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/flags", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /flags status = %d", rec.Code)
	}
	var flags sidecar.FeatureFlags
	if err := json.NewDecoder(rec.Body).Decode(&flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	return flags
}

func TestSidecar_DefaultNominal(t *testing.T) {
	flags := getFlags(t, newSrv())
	if flags.Tier != "nominal" {
		t.Errorf("tier = %q, want nominal", flags.Tier)
	}
	for _, feat := range []string{"recommendations", "analytics", "uploads"} {
		if !flags.Features[feat] {
			t.Errorf("feature %q should be enabled in nominal", feat)
		}
	}
}

func TestSidecar_WarmDisablesRecommendations(t *testing.T) {
	srv := newSrv()
	srv.SetTier("warm", map[string]float64{"rps": 900})

	flags := getFlags(t, srv)
	if flags.Tier != "warm" {
		t.Errorf("tier = %q, want warm", flags.Tier)
	}
	if flags.Features["recommendations"] {
		t.Error("recommendations should be disabled in warm")
	}
	if !flags.Features["uploads"] {
		t.Error("uploads should be enabled in warm")
	}
}

func TestSidecar_HotDisablesAll(t *testing.T) {
	srv := newSrv()
	srv.SetTier("hot", nil)

	flags := getFlags(t, srv)
	for feat, on := range flags.Features {
		if on {
			t.Errorf("feature %q should be disabled in hot", feat)
		}
	}
}

func TestSidecar_SurvivalFallbackActive(t *testing.T) {
	srv := newSrv()
	srv.SetTier("survival", nil)

	if !srv.FallbackActive() {
		t.Error("FallbackActive() should be true in survival tier")
	}
	if srv.Tier() != "survival" {
		t.Errorf("Tier() = %q, want survival", srv.Tier())
	}
}

func TestSidecar_Health(t *testing.T) {
	srv := newSrv()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", rec.Code)
	}
}

func TestSidecar_Override_Valid(t *testing.T) {
	srv := sidecar.NewServer(":0", "tok")
	rec := httptest.NewRecorder()
	body := `{"tier":"hot","token":"tok"}`
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Errorf("override status = %d, want 202", rec.Code)
	}
	if srv.Tier() != "hot" {
		t.Errorf("tier after override = %q, want hot", srv.Tier())
	}
}

func TestSidecar_Override_WrongToken(t *testing.T) {
	srv := sidecar.NewServer(":0", "secret")
	rec := httptest.NewRecorder()
	body := `{"tier":"hot","token":"wrong"}`
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func policyServer(t *testing.T, src string) *sidecar.Server {
	t.Helper()
	p, err := policy.LoadFromBytes([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	res, err := compiler.Compile(p, []byte(src))
	if err != nil {
		t.Fatalf("compile: %v %v", err, res.Diagnostics)
	}
	srv := sidecar.NewServer(":0", "")
	srv.UsePolicy(sidecar.PolicyFromBundle(res.Bundle))
	return srv
}

const customPolicy = `
service: blog
signals: [ { id: rps } ]
tiers:
  - name: calm
    when: { rps: { lt: 100 } }
    behavior: { mode: full_service }
  - name: busy
    when: { rps: { gte: 100, lt: 500 } }
    behavior: { mode: full_service, disable: [comments] }
  - name: frozen
    when: { rps: { gte: 500, lt: 900 } }
    behavior: { mode: read_only }
  - name: down
    when: { rps: { gte: 900 } }
    behavior: { mode: static_fallback }
`

// Regression: flags used to be hard-coded for five tier names, ignoring the policy.
func TestSidecar_FlagsFollowPolicy(t *testing.T) {
	srv := policyServer(t, customPolicy)

	f := getFlags(t, srv)
	if f.Tier != "calm" || f.Mode != "full_service" || !f.Features["comments"] {
		t.Errorf("initial flags = %+v, want calm with comments on", f)
	}
	if _, ok := f.Features["recommendations"]; ok {
		t.Error("features not mentioned by the policy should not appear")
	}

	srv.SetTier("busy", nil)
	if f := getFlags(t, srv); f.Features["comments"] {
		t.Error("comments should be disabled in busy")
	}
	srv.SetTier("frozen", nil)
	if f := getFlags(t, srv); f.Mode != "read_only" || f.Features["comments"] {
		t.Errorf("read_only tier should disable everything: %+v", f)
	}
	if srv.FallbackActive() {
		t.Error("read_only is not static fallback")
	}
	srv.SetTier("down", nil)
	if !srv.FallbackActive() {
		t.Error("static_fallback tier should activate the fallback page")
	}
}

func TestSidecar_OverrideRejectsUnknownTier(t *testing.T) {
	srv := policyServer(t, customPolicy)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/override", strings.NewReader(`{"tier":"banana"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("override to unknown tier = %d, want 400", rec.Code)
	}
	if srv.Tier() != "calm" {
		t.Errorf("tier changed to %q", srv.Tier())
	}
}
