package compiler

import (
	"fmt"
	"math"
	"sort"
	"strconv"

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

	if len(p.Tiers) == 0 {
		diags = append(diags, CompileError{
			Severity: SeverityError,
			Category: "structure",
			Message:  "Policy declares no tiers.",
		})
	}

	seenTiers := make(map[string]bool, len(p.Tiers))
	for _, tier := range p.Tiers {
		diags = append(diags, validateTierShape(tier, seenTiers)...)
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

var (
	validModes     = map[string]bool{"": true, "full_service": true, "read_only": true, "static_fallback": true}
	validOnTimeout = map[string]bool{"": true, "execute_policy": true, "hold": true, "escalate": true}
)

// validateTierShape checks a single tier for naming, mode, gate and range mistakes.
func validateTierShape(tier policy.Tier, seen map[string]bool) []CompileError {
	var diags []CompileError
	add := func(sev Severity, cat, msg string) {
		diags = append(diags, CompileError{Severity: sev, Category: cat, Message: msg})
	}

	if tier.Name == "" {
		add(SeverityError, "structure", "A tier is missing its name.")
	} else if seen[tier.Name] {
		add(SeverityError, "structure", fmt.Sprintf("Tier %q is declared more than once.", tier.Name))
	}
	seen[tier.Name] = true

	if !validModes[tier.Behavior.Mode] {
		add(SeverityError, "behavior", fmt.Sprintf(
			"Tier %q has unknown mode %q (want full_service, read_only or static_fallback).", tier.Name, tier.Behavior.Mode))
	}
	if g := tier.HumanGate; g != nil {
		if !validOnTimeout[g.OnTimeout] {
			add(SeverityError, "gate", fmt.Sprintf(
				"Tier %q has unknown human_gate.on_timeout %q (want execute_policy, hold or escalate).", tier.Name, g.OnTimeout))
		}
		if g.Wait <= 0 {
			add(SeverityError, "gate", fmt.Sprintf("Tier %q declares a human_gate without a positive wait.", tier.Name))
		}
	}
	for _, id := range emptyRanges(tier.When) {
		add(SeverityError, "contradiction", fmt.Sprintf(
			"Tier %q: the comparator on signal %q can never be true (lower bound is above upper bound).", tier.Name, id))
	}
	return diags
}

// emptyRanges returns the signal IDs whose comparator describes an empty range,
// e.g. { gte: 2000, lt: 1500 }.
func emptyRanges(c policy.Condition) []string {
	var out []string
	for id, cmp := range c.Comparisons {
		iv, ok := comparatorToInterval(cmp)
		if !ok {
			continue
		}
		if iv.lo > iv.hi || (iv.lo == iv.hi && !(iv.loIncl && iv.hiIncl)) {
			out = append(out, id)
		}
	}
	for _, sub := range c.AllOf {
		out = append(out, emptyRanges(sub)...)
	}
	for _, sub := range c.AnyOf {
		out = append(out, emptyRanges(sub)...)
	}
	return out
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

// interval is a numeric range with explicit bound inclusivity.
// Missing bounds are represented as ±Inf.
type interval struct {
	lo, hi         float64
	loIncl, hiIncl bool
}

// tierInterval pairs a tier name with its numeric range for gap analysis.
type tierInterval struct {
	tierName string
	iv       interval
}

// detectTierGaps finds uncovered ranges for simple single-signal ladders.
// It only fires when every tier is a flat condition on the same single signal;
// compound (all_of / any_of) ladders need SMT-style analysis and are skipped.
//
// The ladder's lower end is taken from the lowest declared bound (signals are
// usually non-negative, so "lt: 800" covers [0, 800)); values above the highest
// declared bound must also be covered, otherwise they silently fall back to the
// first tier at runtime.
func detectTierGaps(p *policy.DegradationPolicy) []CompileError {
	if len(p.Tiers) < 2 {
		return nil
	}

	var ivs []tierInterval
	signal := ""
	for _, tier := range p.Tiers {
		if len(tier.When.AllOf) > 0 || len(tier.When.AnyOf) > 0 || len(tier.When.Comparisons) != 1 {
			return nil
		}
		for id, cmp := range tier.When.Comparisons {
			if signal != "" && id != signal {
				return nil // ladder spans several signals — out of scope
			}
			signal = id
			iv, ok := comparatorToInterval(cmp)
			if !ok {
				return nil
			}
			ivs = append(ivs, tierInterval{tier.Name, iv})
		}
	}

	sortIntervals(ivs)

	var diags []CompileError
	gap := func(msg string) {
		diags = append(diags, CompileError{Severity: SeverityError, Category: "tier gap", Message: msg})
	}

	// cursor is the highest upper bound covered so far.
	cursor, cursorIncl := ivs[0].iv.hi, ivs[0].iv.hiIncl
	for _, tiv := range ivs[1:] {
		iv := tiv.iv
		switch {
		case iv.lo > cursor:
			gap(fmt.Sprintf("No tier covers %s in %s%s, %s%s — values there fall back to tier %q.",
				signal, bracket(cursorIncl, true), fmtBound(cursor), fmtBound(iv.lo), bracket(iv.loIncl, false), p.Tiers[0].Name))
		case iv.lo == cursor && !cursorIncl && !iv.loIncl:
			gap(fmt.Sprintf("No tier covers %s = %s exactly (one tier uses lt, the next uses gt).", signal, fmtBound(cursor)))
		}
		if iv.hi > cursor || (iv.hi == cursor && iv.hiIncl) {
			cursor, cursorIncl = iv.hi, iv.hiIncl
		}
	}
	if !math.IsInf(cursor, 1) {
		op := "≥"
		if cursorIncl {
			op = ">"
		}
		gap(fmt.Sprintf("No tier covers %s %s %s — values there fall back to tier %q.", signal, op, fmtBound(cursor), p.Tiers[0].Name))
	}
	return diags
}

// comparatorToInterval converts a comparator (which may combine a lower and an
// upper bound, e.g. { gte: 800, lt: 1500 }) into an interval.
func comparatorToInterval(c policy.Comparator) (interval, bool) {
	if c.Eq != nil {
		return interval{lo: *c.Eq, hi: *c.Eq, loIncl: true, hiIncl: true}, true
	}
	iv := interval{lo: math.Inf(-1), hi: math.Inf(1)}
	switch {
	case c.Gte != nil:
		iv.lo, iv.loIncl = *c.Gte, true
	case c.Gt != nil:
		iv.lo = *c.Gt
	}
	switch {
	case c.Lte != nil:
		iv.hi, iv.hiIncl = *c.Lte, true
	case c.Lt != nil:
		iv.hi = *c.Lt
	}
	if math.IsInf(iv.lo, -1) && math.IsInf(iv.hi, 1) {
		return interval{}, false // empty comparator
	}
	return iv, true
}

func bracket(open, left bool) string {
	switch {
	case left && open:
		return "("
	case left:
		return "["
	case open:
		return ")"
	default:
		return "]"
	}
}

func fmtBound(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func sortIntervals(ivs []tierInterval) {
	sort.SliceStable(ivs, func(i, j int) bool { return ivs[i].iv.lo < ivs[j].iv.lo })
}
