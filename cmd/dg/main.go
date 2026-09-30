// Command dg is the IDDC policy compiler and inspection tool.
//
// Usage:
//
//	dg validate <policy.yaml>          — dry-run validation only
//	dg compile  <policy.yaml> [-o out] — validate + emit .dg binary
//	dg inspect  <bundle.dg>            — human-readable dump of compiled bundle
//	dg graph    <bundle.dg>            — ASCII blast-radius graph
//	dg simulate <policy.yaml> [--signal=id:val ...]  — simulate tier without running engine
//	dg version
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"iddc/pkg/compiler"
	"iddc/pkg/policy"
	"iddc/pkg/runtime"
)

var rootCmd = &cobra.Command{
	Use:   "dg",
	Short: "IDDC — Intent-Driven Degradation Contracts compiler and tool",
	// Arguments are validated before this hook runs, so usage is still printed
	// for bad invocations but not for runtime errors such as a failed compile.
	PersistentPreRun: func(cmd *cobra.Command, args []string) { cmd.SilenceUsage = true },
}

func main() {
	rootCmd.AddCommand(
		validateCmd(),
		compileCmd(),
		inspectCmd(),
		graphCmd(),
		simulateCmd(),
		versionCmd(),
	)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// ─── validate ────────────────────────────────────────────────────────────────

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <policy.yaml>",
		Short: "Validate a degradation.yaml without emitting a binary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompile(args[0], "", true)
		},
	}
}

// ─── compile ─────────────────────────────────────────────────────────────────

func compileCmd() *cobra.Command {
	var outPath string
	c := &cobra.Command{
		Use:   "compile <policy.yaml>",
		Short: "Compile a degradation.yaml into a .dg binary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompile(args[0], outPath, false)
		},
	}
	c.Flags().StringVarP(&outPath, "output", "o", "", "Output path for the .dg binary (default: <policy>.dg)")
	return c
}

func runCompile(policyPath, outPath string, dryRun bool) error {
	src, err := os.ReadFile(policyPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", policyPath, err)
	}

	p, err := policy.LoadFromBytes(src)
	if err != nil {
		return fmt.Errorf("parsing policy: %w", err)
	}

	result, err := compiler.Compile(p, src)

	// Print all diagnostics regardless of outcome.
	if len(result.Diagnostics) > 0 {
		fmt.Fprintf(os.Stderr, "\ndiagnostics for %s\n", policyPath)
		for _, d := range result.Diagnostics {
			fmt.Fprintln(os.Stderr, " ", d.Error())
		}
		fmt.Fprintln(os.Stderr)
	}

	if err != nil {
		// Count errors vs warnings.
		errCount, warnCount := 0, 0
		for _, d := range result.Diagnostics {
			if d.Severity == compiler.SeverityError {
				errCount++
			} else {
				warnCount++
			}
		}
		fmt.Fprintf(os.Stderr, "Compilation failed with %d error(s), %d warning(s).\n", errCount, warnCount)
		return err
	}

	if dryRun {
		fmt.Printf("✓  %s is valid\n", policyPath)
		return nil
	}

	if outPath == "" {
		outPath = strings.TrimSuffix(strings.TrimSuffix(policyPath, ".yaml"), ".yml") + ".dg"
	}
	if err := result.Bundle.WriteToFile(outPath); err != nil {
		return err
	}
	fmt.Printf("✓  compiled → %s  (service=%s  tiers=%d)\n",
		outPath, result.Bundle.Service, len(result.Bundle.Tiers))
	return nil
}

// ─── inspect ─────────────────────────────────────────────────────────────────

func inspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <bundle.dg>",
		Short: "Print a human-readable dump of a compiled .dg bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := compiler.ReadBundleFromFile(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Service:      %s\n", b.Service)
			fmt.Printf("Version:      %s\n", b.Version)
			fmt.Printf("Compiled at:  %s\n", b.CompiledAt.Format("2006-01-02T15:04:05Z"))
			fmt.Printf("Checksum:     %x\n", b.SourceChecksum)
			fmt.Printf("Tiers (%d):\n", len(b.Tiers))
			for i, t := range b.Tiers {
				gate := "—"
				if t.GateSpec != nil {
					gate = fmt.Sprintf("wait=%s on_timeout=%s", t.GateSpec.Wait, t.GateSpec.OnTimeout)
				}
				fmt.Printf("  [%d] %-12s  mode=%-14s  gate=%s\n",
					i, t.Name, t.Behavior.Mode, gate)
			}
			fmt.Printf("Signal sources (%d):\n", len(b.SignalSources))
			for _, s := range b.SignalSources {
				fmt.Printf("  %-20s  source=%s\n", s.ID, s.Source)
			}
			return nil
		},
	}
}

// ─── graph ───────────────────────────────────────────────────────────────────

func graphCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "graph <bundle.dg>",
		Short: "Print the blast radius graph as an ASCII tree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := compiler.ReadBundleFromFile(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Blast radius graph for %s\n", b.Service)
			printGraph(b.BlastGraph, b.Service, "", true)
			if len(b.IsolationSet) > 0 {
				fmt.Print("\nIsolation boundaries: ")
				for k := range b.IsolationSet {
					fmt.Printf("%s ", k)
				}
				fmt.Println()
			}
			return nil
		},
	}
}

func printGraph(graph map[string][]string, node, prefix string, last bool) {
	connector := "├── "
	if last {
		connector = "└── "
	}
	if prefix == "" {
		fmt.Printf("%s\n", node)
	} else {
		fmt.Printf("%s%s%s\n", prefix, connector, node)
	}
	children := graph[node]
	childPrefix := prefix + "│   "
	if last {
		childPrefix = prefix + "    "
	}
	for i, child := range children {
		printGraph(graph, child, childPrefix, i == len(children)-1)
	}
}

// ─── simulate ────────────────────────────────────────────────────────────────

func simulateCmd() *cobra.Command {
	var signalFlags []string
	c := &cobra.Command{
		Use:   "simulate <policy.yaml>",
		Short: "Simulate tier evaluation with given signal values (no engine required)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			p, err := policy.LoadFromBytes(src)
			if err != nil {
				return err
			}
			result, err := compiler.Compile(p, src)
			if err != nil {
				for _, d := range result.Diagnostics {
					fmt.Fprintln(os.Stderr, " ", d.Error())
				}
				return fmt.Errorf("policy invalid: %w", err)
			}

			declared := make(map[string]bool, len(p.Signals))
			for _, s := range p.Signals {
				declared[s.ID] = true
			}
			vals := make(map[string]float64)
			for _, s := range signalFlags {
				parts := strings.SplitN(s, ":", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid --signal flag: %q (expected id:value)", s)
				}
				v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
				if err != nil {
					return fmt.Errorf("invalid value for signal %q: %q is not a number", parts[0], parts[1])
				}
				if !declared[parts[0]] {
					fmt.Fprintf(os.Stderr, "warning: signal %q is not declared in %s and is ignored by every tier\n", parts[0], args[0])
				}
				vals[parts[0]] = v
			}

			// Raw match, no hysteresis: what the engine would converge to.
			tier := runtime.MatchTier(result.Bundle, vals)
			fmt.Printf("Signals: %v\n", vals)
			fmt.Printf("Matched tier: %s\n", tier)
			for _, t := range result.Bundle.Tiers {
				if t.Name != tier {
					continue
				}
				mode := t.Behavior.Mode
				if mode == "" {
					mode = "full_service"
				}
				fmt.Printf("  mode: %s\n", mode)
				if len(t.Behavior.Disable) > 0 {
					fmt.Printf("  disables: %s\n", strings.Join(t.Behavior.Disable, ", "))
				}
				if t.GateSpec != nil {
					fmt.Printf("  human gate: wait=%s on_timeout=%s (the engine holds the transition until decided)\n",
						t.GateSpec.Wait, t.GateSpec.OnTimeout)
				}
			}
			return nil
		},
	}
	c.Flags().StringArrayVar(&signalFlags, "signal", nil, "Signal value: id:value (repeatable)")
	return c
}

// ─── version ─────────────────────────────────────────────────────────────────

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print dg version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("dg v0.1.0 — Intent-Driven Degradation Contracts")
		},
	}
}
