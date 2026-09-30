package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/compiler"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/runtime"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/sigfetch"
)

func exampleBundle(t *testing.T) *compiler.CompiledBundle {
	t.Helper()
	src, err := os.ReadFile("../../config/example-degradation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.LoadFromBytes(src)
	if err != nil {
		t.Fatal(err)
	}
	res, err := compiler.Compile(p, src)
	if err != nil {
		t.Fatalf("compile: %v %v", err, res.Diagnostics)
	}
	return res.Bundle
}

// Every tier of the example policy gets a one-click console scenario, and
// each scenario really lands on its tier.
func TestScenarioForEveryExampleTier(t *testing.T) {
	b := exampleBundle(t)
	for i, tier := range b.Tiers {
		vals := scenarioFor(b, i)
		if vals == nil {
			t.Errorf("no scenario for tier %s", tier.Name)
			continue
		}
		if got := runtime.MatchTier(b, vals); got != tier.Name {
			t.Errorf("scenario %v for %s matches %s", vals, tier.Name, got)
		}
	}
}

func TestPolicyViewQueueFields(t *testing.T) {
	v := describeBundle(exampleBundle(t), nil)
	q := v.Tiers[3].Queue
	if q == nil || q.MaxDepth != 2000 || q.TTL != "2m0s" {
		t.Errorf("critical queue view = %+v", q)
	}
	data, _ := json.Marshal(v.Tiers[3])
	if !strings.Contains(string(data), `"queue_writes":{"max_depth":2000,"ttl":"2m0s"}`) {
		t.Errorf("queue JSON = %s", data)
	}
}

func TestDescribeCondition(t *testing.T) {
	b := exampleBundle(t)
	want := map[string]string{
		"nominal":  "rps < 800",
		"hot":      "rps ≥ 1500 and < 2000 AND pod_ceiling ≥ 0.85",
		"survival": "rps ≥ 10000 OR mem_pressure ≥ 0.95",
	}
	for _, tier := range b.Tiers {
		if w, ok := want[tier.Name]; ok {
			if got := describeCondition(tier.Condition); got != w {
				t.Errorf("%s: %q, want %q", tier.Name, got, w)
			}
		}
	}
}

func TestInjectRejectsUnknownSignals(t *testing.T) {
	store := sigfetch.NewInjectedStore()
	c := &controlAPI{store: store, injectable: map[string]bool{"rps": true}}
	h := c.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/inject", strings.NewReader(`{"rps": 5, "typo_rps": 9}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "typo_rps") {
		t.Errorf("inject with undeclared signal: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Get()["rps"]; ok {
		t.Error("a rejected request must not partially apply")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/inject", strings.NewReader(`{"rps": 5}`)))
	if rec.Code != http.StatusOK || store.Get()["rps"] != 5 {
		t.Errorf("valid inject: %d %v", rec.Code, store.Get())
	}
}

func TestValidateAndSimulateEndpoints(t *testing.T) {
	c := &controlAPI{bundle: exampleBundle(t)}
	h := c.routes()

	gap := "service: s\nsignals: [ {id: rps} ]\ntiers:\n  - {name: a, when: {rps: {lt: 800}}}\n  - {name: b, when: {rps: {gte: 1200}}}\n"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader(gap)))
	var v validateView
	json.NewDecoder(rec.Body).Decode(&v)
	if v.Valid || len(v.Diagnostics) == 0 || v.Diagnostics[0].Category != "tier gap" {
		t.Errorf("validate gap policy = %+v", v)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader("tiers: [")))
	json.NewDecoder(rec.Body).Decode(&v)
	if v.ParseError == "" {
		t.Error("malformed YAML should report parse_error")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/simulate",
		strings.NewReader(`{"signals": {"rps": 100, "mem_pressure": 0.97}}`)))
	var sim struct {
		Tier string `json:"tier"`
		Mode string `json:"mode"`
	}
	json.NewDecoder(rec.Body).Decode(&sim)
	if sim.Tier != "survival" || sim.Mode != "static_fallback" {
		t.Errorf("simulate against running policy = %+v", sim)
	}
}
