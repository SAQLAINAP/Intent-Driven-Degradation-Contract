// Package policy defines the core data types for the IDDC DSL.
// These structs are the in-memory representation of a degradation.yaml file.
package policy

import "time"

// DegradationPolicy is the top-level object parsed from degradation.yaml.
type DegradationPolicy struct {
	Service     string            `yaml:"service"`
	Version     string            `yaml:"version"`
	Signals     []Signal          `yaml:"signals"`
	Tiers       []Tier            `yaml:"tiers"`
	BlastRadius BlastRadiusConfig `yaml:"blast_radius"`
}

// Signal declares a named metric that tiers can reference in their conditions.
type Signal struct {
	ID          string        `yaml:"id"`
	Source      string        `yaml:"source,omitempty"`       // "prometheus", "proc", "http"
	URL         string        `yaml:"url,omitempty"`          // HTTP source: endpoint URL
	Field       string        `yaml:"field,omitempty"`        // HTTP source: JSON field name
	PollCadence time.Duration `yaml:"poll_cadence,omitempty"`
}

// Tier represents one level in the degradation ladder.
type Tier struct {
	Name      string    `yaml:"name"`
	When      Condition `yaml:"when"`
	Behavior  Behavior  `yaml:"behavior"`
	HumanGate *GateSpec `yaml:"human_gate,omitempty"`
}

// Condition is a recursive boolean expression over signal values.
// Exactly one of the fields should be set.
type Condition struct {
	// Flat single-signal comparisons: map of signal ID → comparator rule.
	// e.g.   rps: { lt: 800 }
	Comparisons map[string]Comparator `yaml:",inline"`

	// Compound logic
	AllOf []Condition `yaml:"all_of,omitempty"`
	AnyOf []Condition `yaml:"any_of,omitempty"`
}

// Comparator holds the threshold for a single signal.
// At most one field should be set.
type Comparator struct {
	Lt   *float64 `yaml:"lt,omitempty"`
	Lte  *float64 `yaml:"lte,omitempty"`
	Gt   *float64 `yaml:"gt,omitempty"`
	Gte  *float64 `yaml:"gte,omitempty"`
	Eq   *float64 `yaml:"eq,omitempty"`
}

// Behavior describes what the system does when a tier is active.
type Behavior struct {
	Mode       string      `yaml:"mode,omitempty"` // "full_service", "read_only", "static_fallback"
	Disable    []string    `yaml:"disable,omitempty"`
	QueueWrites *QueueSpec `yaml:"queue_writes,omitempty"`
	Replicas   *int        `yaml:"replicas,omitempty"`
}

// QueueSpec configures the write queue used in degraded modes.
type QueueSpec struct {
	MaxDepth int           `yaml:"max_depth"`
	TTL      time.Duration `yaml:"ttl"`
}

// GateSpec defines the human-in-the-loop gate for a tier.
type GateSpec struct {
	Notify    []string      `yaml:"notify"`
	Wait      time.Duration `yaml:"wait"`
	OnTimeout string        `yaml:"on_timeout"` // "execute_policy", "hold", "escalate"
	OnResponse string       `yaml:"on_response"` // "await_instruction"
}

// BlastRadiusConfig declares service dependencies and isolation boundaries.
type BlastRadiusConfig struct {
	DependsOn          []string `yaml:"depends_on,omitempty"`
	CascadeTo          []string `yaml:"cascade_to,omitempty"`
	IsolationBoundary  string   `yaml:"isolation_boundary,omitempty"`
	RecoverySLASeconds int      `yaml:"recovery_sla_seconds,omitempty"`
}
