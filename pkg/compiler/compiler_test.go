package compiler_test

import (
	"os"
	"strings"
	"testing"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/compiler"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
)

// helper: load policy from inline YAML bytes and compile.
func compileYAML(t *testing.T, src string) (*compiler.Result, error) {
	t.Helper()
	p, err := policy.LoadFromBytes([]byte(src))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return compiler.Compile(p, []byte(src))
}

func hasError(diags []compiler.CompileError, category string) bool {
	for _, d := range diags {
		if d.Severity == compiler.SeverityError && d.Category == category {
			return true
		}
	}
	return false
}

func hasWarning(diags []compiler.CompileError, category string) bool {
	for _, d := range diags {
		if d.Severity == compiler.SeverityWarning && d.Category == category {
			return true
		}
	}
	return false
}

// TC-C-01: valid 5-tier policy compiles cleanly.
func TestCompile_ValidPolicy(t *testing.T) {
	src := `
service: api-gateway
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: nominal
    when:
      rps: { lt: 800 }
    behavior: { mode: full_service }
  - name: critical
    when:
      rps: { gte: 800 }
    behavior: { mode: read_only }
blast_radius: {}
`
	result, err := compileYAML(t, src)
	if err != nil {
		t.Fatalf("expected clean compile, got: %v — diags: %v", err, result.Diagnostics)
	}
	if result.Bundle == nil {
		t.Fatal("expected non-nil bundle")
	}
	if result.Bundle.Service != "api-gateway" {
		t.Errorf("service = %q, want api-gateway", result.Bundle.Service)
	}
}

// TC-C-02: tier gap fires an error.
func TestCompile_TierGap(t *testing.T) {
	src := `
service: svc
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: nominal
    when:
      rps: { lt: 800 }
    behavior: { mode: full_service }
  - name: critical
    when:
      rps: { gte: 1200 }
    behavior: { mode: read_only }
blast_radius: {}
`
	result, _ := compileYAML(t, src)
	if !hasError(result.Diagnostics, "tier gap") {
		t.Errorf("expected 'tier gap' error, got: %v", result.Diagnostics)
	}
}

// TC-C-03: undeclared signal reference.
func TestCompile_UndeclaredSignal(t *testing.T) {
	src := `
service: svc
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: critical
    when:
      db_latency_p99: { gte: 2000 }
    behavior: { mode: read_only }
blast_radius: {}
`
	result, _ := compileYAML(t, src)
	if !hasError(result.Diagnostics, "undeclared") {
		t.Errorf("expected 'undeclared' error, got: %v", result.Diagnostics)
	}
}

// TC-C-04: service in both cascade_to and isolation_boundary.
func TestCompile_IsolationContradiction(t *testing.T) {
	src := `
service: svc
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: nominal
    when:
      rps: { lt: 800 }
    behavior: { mode: full_service }
blast_radius:
  cascade_to: [payment-service]
  isolation_boundary: payment-service
`
	result, _ := compileYAML(t, src)
	if !hasError(result.Diagnostics, "contradiction") {
		t.Errorf("expected 'contradiction' error, got: %v", result.Diagnostics)
	}
}

// TC-C-05: cycle in blast radius graph.
func TestCompile_BlastCycle(t *testing.T) {
	src := `
service: A
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: nominal
    when:
      rps: { lt: 800 }
    behavior: { mode: full_service }
blast_radius:
  depends_on: [B]
  cascade_to: [C]
`
	// Build a direct cycle A → B → A by depending on something that depends back.
	// For the test we'll do it with cascade + depends creating A→C, C→A manually
	// by putting A in cascade_to (service self references).
	src2 := `
service: A
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: nominal
    when:
      rps: { lt: 800 }
    behavior: { mode: full_service }
blast_radius:
  depends_on: [A]
`
	result, _ := compileYAML(t, src2)
	_ = src // suppress unused warning
	if !hasError(result.Diagnostics, "cycle") {
		t.Errorf("expected 'cycle' error, got: %v", result.Diagnostics)
	}
}

// TC-C-06: write queue TTL < recovery SLA emits a warning.
func TestCompile_TTLWarning(t *testing.T) {
	src := `
service: svc
version: "1.0.0"
signals:
  - id: rps
tiers:
  - name: critical
    when:
      rps: { gte: 0 }
    behavior:
      mode: read_only
      queue_writes:
        max_depth: 1000
        ttl: 30s
blast_radius:
  recovery_sla_seconds: 120
`
	result, _ := compileYAML(t, src)
	if !hasWarning(result.Diagnostics, "ttl-sla") {
		t.Errorf("expected 'ttl-sla' warning, got: %v", result.Diagnostics)
	}
}

// Gob round-trip: compile → serialize → deserialize → fields intact.
func TestCompile_GobRoundTrip(t *testing.T) {
	src := `
service: round-trip
version: "2.0.0"
signals:
  - id: rps
tiers:
  - name: nominal
    when:
      rps: { lt: 100 }
    behavior: { mode: full_service }
blast_radius: {}
`
	result, err := compileYAML(t, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	data, err := result.Bundle.MarshalBytes()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b2, err := compiler.UnmarshalBundle(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b2.Service != "round-trip" {
		t.Errorf("service = %q, want round-trip", b2.Service)
	}
	if b2.Version != "2.0.0" {
		t.Errorf("version = %q, want 2.0.0", b2.Version)
	}
	if b2.SourceChecksum != result.Bundle.SourceChecksum {
		t.Error("checksum mismatch after round-trip")
	}
}

func compileOK(t *testing.T, src string) *compiler.Result {
	t.Helper()
	result, err := compileYAML(t, src)
	if err != nil {
		t.Fatalf("expected clean compile, got %v — %v", err, result.Diagnostics)
	}
	return result
}

// Regression: ranges written as { gte: a, lt: b } used to lose their lower
// bound, so a gap between two range tiers went undetected.
func TestCompile_TierGapWithRangeComparator(t *testing.T) {
	src := `
service: svc
signals: [ { id: rps } ]
tiers:
  - name: nominal
    when: { rps: { lt: 800 } }
  - name: warm
    when: { rps: { gte: 1200, lt: 2000 } }
  - name: critical
    when: { rps: { gte: 2000 } }
`
	result, _ := compileYAML(t, src)
	if !hasError(result.Diagnostics, "tier gap") {
		t.Fatalf("expected tier gap for [800, 1200), got %v", result.Diagnostics)
	}
	if msg := result.Diagnostics[0].Message; !strings.Contains(msg, "[800, 1200)") {
		t.Errorf("gap message = %q, want it to name [800, 1200)", msg)
	}
}

func TestCompile_TierGapAboveTopTier(t *testing.T) {
	src := `
service: svc
signals: [ { id: rps } ]
tiers:
  - name: nominal
    when: { rps: { lt: 800 } }
  - name: warm
    when: { rps: { gte: 800, lt: 1500 } }
`
	result, _ := compileYAML(t, src)
	if !hasError(result.Diagnostics, "tier gap") {
		t.Errorf("expected tier gap for rps ≥ 1500, got %v", result.Diagnostics)
	}
}

func TestCompile_AdjacentInclusiveBoundsHaveNoGap(t *testing.T) {
	compileOK(t, `
service: svc
signals: [ { id: mem } ]
tiers:
  - name: nominal
    when: { mem: { lte: 0.8 } }
  - name: warm
    when: { mem: { gt: 0.8, lt: 0.95 } }
  - name: survival
    when: { mem: { gte: 0.95 } }
`)
}

func TestCompile_PointGapBetweenLtAndGt(t *testing.T) {
	result, _ := compileYAML(t, `
service: svc
signals: [ { id: rps } ]
tiers:
  - name: nominal
    when: { rps: { lt: 800 } }
  - name: warm
    when: { rps: { gt: 800 } }
`)
	if !hasError(result.Diagnostics, "tier gap") {
		t.Errorf("expected a gap at rps = 800 exactly, got %v", result.Diagnostics)
	}
}

func TestCompile_ShapeErrors(t *testing.T) {
	cases := map[string]string{
		"duplicate tier": `
service: svc
signals: [ { id: rps } ]
tiers:
  - { name: a, when: { rps: { lt: 1 } } }
  - { name: a, when: { rps: { gte: 1 } } }
`,
		"unknown mode": `
service: svc
signals: [ { id: rps } ]
tiers:
  - { name: a, when: { rps: { gte: 0 } }, behavior: { mode: readonly } }
`,
		"bad on_timeout": `
service: svc
signals: [ { id: rps } ]
tiers:
  - { name: a, when: { rps: { gte: 0 } }, human_gate: { wait: 5s, on_timeout: explode } }
`,
		"empty range": `
service: svc
signals: [ { id: rps } ]
tiers:
  - { name: a, when: { any_of: [ { rps: { gte: 2000, lt: 1500 } } ] } }
`,
		"no tiers": `
service: svc
signals: [ { id: rps } ]
`,
	}
	for name, src := range cases {
		if _, err := compileYAML(t, src); err == nil {
			t.Errorf("%s: expected a compile error", name)
		}
	}
}

func TestCompile_ExampleLadderIsClean(t *testing.T) {
	data, err := os.ReadFile("../../config/example-degradation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result := compileOK(t, string(data))
	if n := len(result.Bundle.Tiers); n != 5 {
		t.Errorf("example tiers = %d, want 5", n)
	}
}
