package compiler

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"time"

	"iddc/pkg/policy"
)

// CompiledBundle is the serialized artifact produced by the compiler.
// It is stored as a gob-encoded binary (.dg file).
type CompiledBundle struct {
	Version        string
	Service        string
	CompiledAt     time.Time
	SourceChecksum [32]byte
	Tiers          []CompiledTier
	BlastGraph     map[string][]string // adjacency list: service → dependencies
	IsolationSet   map[string]bool     // O(1) lookup for isolation boundaries
	SignalSources  []SignalSource
}

// CompiledTier is a validated tier with its condition tree preserved.
type CompiledTier struct {
	Name      string
	Condition policy.Condition
	Behavior  policy.Behavior
	GateSpec  *policy.GateSpec
}

// SignalSource maps a signal ID to its collection source.
type SignalSource struct {
	ID     string
	Source string
	URL    string // HTTP source: endpoint URL
	Field  string // HTTP source: JSON field to extract
}

func init() {
	// Register types that appear in interface values inside gob encoding.
	gob.Register(map[string]interface{}{})
	gob.Register([]interface{}{})
}

// WriteTo gob-encodes the bundle to the given writer.
// It implements io.WriterTo.
func (b *CompiledBundle) WriteTo(w io.Writer) (int64, error) {
	enc := gob.NewEncoder(w)
	if err := enc.Encode(b); err != nil {
		return 0, fmt.Errorf("encoding bundle: %w", err)
	}
	return 0, nil
}

// WriteToFile serializes the bundle to a file at path.
func (b *CompiledBundle) WriteToFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating output file %s: %w", path, err)
	}
	defer f.Close()
	_, err = b.WriteTo(f)
	return err
}

// ReadBundle deserializes a CompiledBundle from the given reader.
func ReadBundle(r io.Reader) (*CompiledBundle, error) {
	var b CompiledBundle
	dec := gob.NewDecoder(r)
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("decoding bundle: %w", err)
	}
	return &b, nil
}

// ReadBundleFromFile loads a .dg bundle from disk.
func ReadBundleFromFile(path string) (*CompiledBundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening bundle file %s: %w", path, err)
	}
	defer f.Close()
	return ReadBundle(f)
}

// MarshalBytes encodes the bundle to an in-memory byte slice.
func (b *CompiledBundle) MarshalBytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := b.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalBundle decodes a bundle from raw bytes.
func UnmarshalBundle(data []byte) (*CompiledBundle, error) {
	return ReadBundle(bytes.NewReader(data))
}
