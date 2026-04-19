package compiler

import (
	"fmt"
	"math"

	"iddc/pkg/policy"
)

// validate runs Stage 2 semantic checks on the parsed policy.
// Returns a slice of CompileErrors (may include warnings).
func validate(p *policy.DegradationPolicy) []CompileError {
	var diags []CompileError

	// Build a set of declared signal IDs for O(1) lookup.
	declaredSignals := make(map[string]bool, len(p.Signals))
	for _, s := range p.Signals {
		declaredSignals[s.ID] = true
	}

	// Check each tier's condition references only declared signals.
	for _, tier := range p.Tiers {
		refs := collectSignalRefs(tier.When)
		for _, ref := range refs {
			if !declaredSignals[ref] {
				diags = append(diags, CompileError{
					Severity: SeverityError,
					Category: "undeclared",
					Message:  fmt.Sprintf("Tier %q references signal %q but it is not declared in signals:", tier.Name, ref),
				})
			}
		}
		// Check write queue TTL vs recovery SLA
		if tier.Behavior.QueueWrites != nil && p.BlastRadius.RecoverySLASeconds > 0 {
			ttlSec := int(tier.Behavior.QueueWrites.TTL.Seconds())
			if ttlSec > 0 && ttlSec < p.BlastRadius.RecoverySLASeconds {
				diags = append(diags, CompileError{
					Severity: SeverityWarning,
					Category: "ttl-sla",
					Message: fmt.Sprintf(
						"Tier %q write queue TTL (%ds) < estimated DB recovery SLA (%ds). Writes may be dropped before recovery.",
						tier.Name, ttlSec, p.BlastRadius.RecoverySLASeconds,
					),
				})
			}
		}
	}

	// Tier gap detection: for any single-signal tier using a single comparator,
	// check that the declared ranges cover the full space without gaps.
	// We do a simplified scan over the primary signal (first declared).
	// Full multi-signal interval analysis would require SMT; this covers the common case.
	diags = append(diags, detectTierGaps(p)...)

	return diags
}

// collectSignalRefs recursively walks a Condition tree and returns all signal IDs referenced.
func collectSignalRefs(c policy.Condition) []string {
	var refs []string
	for id := range c.Comparisons {
		refs = append(refs, id)
	}
	for _, sub := range c.AllOf {
		refs = append(refs, collectSignalRefs(sub)...)
	}
	for _, sub := range c.AnyOf {
		refs = append(refs, collectSignalRefs(sub)...)
	}
	return refs
}

// interval represents a numeric half-open range [lo, hi).
type interval struct {
	lo float64
	hi float64
}

// tierInterval pairs a tier name with its numeric range for gap analysis.
type tierInterval struct {
	tierName string
	iv       interval
}

// detectTierGaps finds uncovered ranges for simple single-signal tiers.
// It only fires if all tiers use the same single signal with exactly one comparator.
func detectTierGaps(p *policy.DegradationPolicy) []CompileError {
	if len(p.Tiers) < 2 {
		return nil
	}

	// Collect intervals from flat single-signal conditions.
	var ivs []tierInterval

	for _, tier := range p.Tiers {
		// Only handle flat single-signal conditions (no AllOf/AnyOf) for gap analysis.
		if len(tier.When.AllOf) > 0 || len(tier.When.AnyOf) > 0 {
			return nil
		}
		if len(tier.When.Comparisons) != 1 {
			return nil
		}
		for _, cmp := range tier.When.Comparisons {
			iv := comparatorToInterval(cmp)
			if iv == nil {
				return nil
			}
			ivs = append(ivs, tierInterval{tier.Name, *iv})
		}
	}

	if len(ivs) == 0 {
		return nil
	}

	// Sort by lower bound and look for gaps.
	sortIntervals(ivs)
	var diags []CompileError
	cursor := ivs[0].iv.lo
	for _, tiv := range ivs {
		if tiv.iv.lo > cursor+1e-9 {
			diags = append(diags, CompileError{
				Severity: SeverityError,
				Category: "tier gap",
				Message:  fmt.Sprintf("No tier covers the range [%.0f, %.0f).", cursor, tiv.iv.lo),
			})
		}
		if tiv.iv.hi > cursor {
			cursor = tiv.iv.hi
		}
	}
	return diags
}

func comparatorToInterval(c policy.Comparator) *interval {
	switch {
	case c.Lt != nil:
		return &interval{lo: 0, hi: *c.Lt}
	case c.Lte != nil:
		return &interval{lo: 0, hi: *c.Lte + 1}
	case c.Gte != nil:
		return &interval{lo: *c.Gte, hi: math.MaxFloat64}
	case c.Gt != nil:
		return &interval{lo: *c.Gt + 1, hi: math.MaxFloat64}
	default:
		return nil
	}
}

func sortIntervals(ivs []tierInterval) {
	// Simple insertion sort — tier counts are small (≤10).
	for i := 1; i < len(ivs); i++ {
		for j := i; j > 0 && ivs[j].iv.lo < ivs[j-1].iv.lo; j-- {
			ivs[j], ivs[j-1] = ivs[j-1], ivs[j]
		}
	}
}
