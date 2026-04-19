package compiler_test

import (
	"testing"

	"iddc/pkg/compiler"
	"iddc/pkg/policy"
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
