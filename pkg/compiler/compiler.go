// Package compiler implements the four-stage dg policy compiler.
//
// Stage 1 — Parse:   YAML → DegradationPolicy (handled by pkg/policy)
// Stage 2 — Validate: semantic checks (tier gaps, signal refs, contradictions)
// Stage 3 — Blast:   dependency graph cycle detection + reachability
// Stage 4 — Emit:    gob-encode CompiledBundle → .dg binary
package compiler

import (
	"crypto/sha256"
	"fmt"
	"time"

	"iddc/pkg/policy"
)

// Severity classifies a compile diagnostic.
type Severity string

const (
	SeverityError   Severity = "ERROR"
	SeverityWarning Severity = "WARN"
)

// CompileError is a structured diagnostic returned by the compiler.
type CompileError struct {
	Severity Severity
	Category string // e.g. "tier gap", "undeclared", "contradiction", "cycle"
	Message  string
}

func (e CompileError) Error() string {
	return fmt.Sprintf("%-7s [%s] %s", e.Severity, e.Category, e.Message)
}

// Result holds the output of a successful compilation.
// Diagnostics may still contain warnings even when Err == nil.
type Result struct {
	Bundle      *CompiledBundle
	Diagnostics []CompileError
}

// Compile runs all four stages against the given policy.
// It returns a Result plus any fatal errors.
// If len(errors) > 0 where any error has SeverityError, Bundle will be nil.
func Compile(p *policy.DegradationPolicy, sourceYAML []byte) (*Result, error) {
	result := &Result{}

	// Stage 2 — Validate
	diags := validate(p)

	// Stage 3 — Blast radius
	blastDiags, graph, isolationSet := analyzeBlast(p)
	diags = append(diags, blastDiags...)

	result.Diagnostics = diags

	// If any error-level diagnostic exists, do not emit a bundle.
	for _, d := range diags {
		if d.Severity == SeverityError {
			return result, fmt.Errorf("compilation failed with errors")
		}
	}

	// Stage 4 — Emit
	checksum := sha256.Sum256(sourceYAML)
	bundle := &CompiledBundle{
		Version:        p.Version,
		Service:        p.Service,
		CompiledAt:     time.Now().UTC(),
		SourceChecksum: checksum,
		Tiers:          compileTiers(p.Tiers),
		BlastGraph:     graph,
		IsolationSet:   isolationSet,
		SignalSources:  compileSignalSources(p.Signals),
	}
	result.Bundle = bundle
	return result, nil
}

func compileTiers(tiers []policy.Tier) []CompiledTier {
	out := make([]CompiledTier, len(tiers))
	for i, t := range tiers {
		out[i] = CompiledTier{
			Name:      t.Name,
			Condition: t.When,
			Behavior:  t.Behavior,
			GateSpec:  t.HumanGate,
		}
	}
	return out
}

func compileSignalSources(signals []policy.Signal) []SignalSource {
	out := make([]SignalSource, len(signals))
	for i, s := range signals {
		src := s.Source
		if src == "" {
			src = "proc"
		}
		out[i] = SignalSource{
			ID:     s.ID,
			Source: src,
			URL:    s.URL,
			Field:  s.Field,
		}
	}
	return out
}
