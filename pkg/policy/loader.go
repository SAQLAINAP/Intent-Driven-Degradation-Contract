package policy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadFromFile reads and parses a degradation.yaml file into a DegradationPolicy.
func LoadFromFile(path string) (*DegradationPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file: %w", err)
	}
	return LoadFromBytes(data)
}

// LoadFromBytes parses raw YAML bytes into a DegradationPolicy.
func LoadFromBytes(data []byte) (*DegradationPolicy, error) {
	var p DegradationPolicy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing policy YAML: %w", err)
	}
	return &p, nil
}
