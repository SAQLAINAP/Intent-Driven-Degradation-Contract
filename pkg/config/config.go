// Package config loads and parses dg-engine.yaml into a typed Config struct.
// Environment variable substitution is supported: ${VAR_NAME} in values
// is replaced with the value of that env var at load time.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level runtime configuration for dg-engine.
type Config struct {
	Server     ServerConfig    `yaml:"server"`
	SignalBus  SignalBusConfig `yaml:"signal_bus"`
	Evaluator  EvaluatorConfig `yaml:"evaluator"`
	Gate       GateConfig      `yaml:"gate"`
	Audit      AuditConfig     `yaml:"audit"`
	Metrics    MetricsConfig   `yaml:"metrics"`
	K8s        K8sConfig       `yaml:"k8s"`
	PolicyPath string          `yaml:"policy_path"` // path to the .dg bundle
}

// ServerConfig sets the listen addresses of the engine's HTTP servers.
type ServerConfig struct {
	ControlAddr string `yaml:"control_addr"` // control API + web console (inject, override, status)
	SidecarAddr string `yaml:"sidecar_addr"` // feature-flag sidecar queried by applications
	GateAddr    string `yaml:"gate_addr"`    // human-gate callbacks (Slack buttons, curl)
}

// SignalBusConfig controls polling behaviour.
type SignalBusConfig struct {
	TickInterval       time.Duration `yaml:"tick_interval"`
	StaleSignalTimeout time.Duration `yaml:"stale_signal_timeout"`
	// Mode selects where signal values come from:
	//   auto   — each signal uses its declared source (prometheus needs
	//            metrics.prometheus_url, proc needs Linux); anything that
	//            can't be collected falls back to injected values.
	//   inject — every signal is driven by POST /inject (demos, tests).
	Mode string `yaml:"mode"`
}

// EvaluatorConfig controls hysteresis windows.
type EvaluatorConfig struct {
	UpHysteresis   time.Duration `yaml:"up_hysteresis"`
	DownHysteresis time.Duration `yaml:"down_hysteresis"`
}

// GateConfig holds credentials for the human gate.
type GateConfig struct {
	SlackWebhookURL     string `yaml:"slack_webhook_url"`
	CallbackBaseURL     string `yaml:"callback_base_url"`
	AuthToken           string `yaml:"auth_token"`
	PagerDutyRoutingKey string `yaml:"pagerduty_routing_key"`
}

// AuditConfig controls the audit log output.
type AuditConfig struct {
	Output string `yaml:"output"` // "stdout" or a file path
	Format string `yaml:"format"` // "ndjson"
}

// MetricsConfig controls the Prometheus metrics server.
type MetricsConfig struct {
	PrometheusPort int    `yaml:"prometheus_port"`
	PrometheusURL  string `yaml:"prometheus_url"` // URL of Prometheus for signal fetching
}

// K8sConfig controls Kubernetes integration.
type K8sConfig struct {
	Namespace      string `yaml:"namespace"`
	DeploymentName string `yaml:"deployment_name"`
	KubeconfigPath string `yaml:"kubeconfig_path"`
	TargetWarm     int32  `yaml:"target_warm"`
	MaxPods        int32  `yaml:"max_pods"`
	LeaderElection bool   `yaml:"leader_election"`
}

// Defaults returns a Config pre-filled with sensible defaults.
// Values from a file override these.
func Defaults() Config {
	return Config{
		Server: ServerConfig{
			ControlAddr: ":9191",
			SidecarAddr: ":8081",
			GateAddr:    ":9292",
		},
		SignalBus: SignalBusConfig{
			TickInterval:       2 * time.Second,
			StaleSignalTimeout: 30 * time.Second,
			Mode:               "auto",
		},
		Evaluator: EvaluatorConfig{
			UpHysteresis:   30 * time.Second,
			DownHysteresis: 90 * time.Second,
		},
		Audit: AuditConfig{
			Output: "stdout",
			Format: "ndjson",
		},
		Metrics: MetricsConfig{
			PrometheusPort: 9090,
		},
		K8s: K8sConfig{
			Namespace: "default",
		},
	}
}

// envVarRe matches ${VAR_NAME} patterns in YAML values.
var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// LoadFromFile reads dg-engine.yaml, substitutes ${ENV_VAR} placeholders (unset → empty),
// and merges the result on top of Defaults().
func LoadFromFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	return LoadFromBytes(raw)
}

// LoadFromBytes parses YAML bytes into a Config, applying env-var substitution
// and merging on top of Defaults().
func LoadFromBytes(raw []byte) (*Config, error) {
	// Substitute ${ENV_VAR} placeholders.
	// Unset variables expand to "" so optional integrations (Slack, PagerDuty,
	// auth token) are disabled rather than configured with a literal "${VAR}".
	expanded := envVarRe.ReplaceAllFunc(raw, func(match []byte) []byte {
		varName := string(match[2 : len(match)-1]) // strip ${ and }
		return []byte(os.Getenv(varName))
	})

	cfg := Defaults()
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}
