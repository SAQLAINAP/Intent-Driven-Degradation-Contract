package compiler

import (
	"fmt"
	"strings"

	"iddc/pkg/policy"
)

// analyzeBlast runs Stage 3: builds the dependency graph, detects cycles via DFS,
// checks isolation boundary contradictions, and returns a reachability adjacency list.
func analyzeBlast(p *policy.DegradationPolicy) (diags []CompileError, graph map[string][]string, isolationSet map[string]bool) {
	br := p.BlastRadius
	graph = make(map[string][]string)
	isolationSet = make(map[string]bool)

	// Build the directed graph: service → what it depends on + what it cascades to.
	svc := p.Service
	if svc == "" {
		svc = "self"
	}

	all := append(br.DependsOn, br.CascadeTo...)
	graph[svc] = all
	for _, dep := range all {
		if _, exists := graph[dep]; !exists {
			graph[dep] = nil
		}
	}

	// Isolation boundary contradiction: a service cannot be both in cascade_to and be the isolation boundary.
	if br.IsolationBoundary != "" {
		isolationSet[br.IsolationBoundary] = true
		for _, cascade := range br.CascadeTo {
			if cascade == br.IsolationBoundary {
				diags = append(diags, CompileError{
					Severity: SeverityError,
					Category: "contradiction",
					Message: fmt.Sprintf(
						"%q is in both cascade_to and isolation_boundary — a service cannot cascade through its own isolation fence.",
						br.IsolationBoundary,
					),
				})
			}
		}
	}

	// DFS cycle detection using white (0) / grey (1) / black (2) colouring.
	color := make(map[string]int, len(graph))
	var cyclePath []string
	var dfs func(node string) bool

	dfs = func(node string) bool {
		color[node] = 1 // grey — currently visiting
		for _, neighbour := range graph[node] {
			if color[neighbour] == 1 {
				// Back edge — cycle detected.
				cyclePath = append(cyclePath, node, neighbour)
				return true
			}
			if color[neighbour] == 0 {
				if dfs(neighbour) {
					return true
				}
			}
		}
		color[node] = 2 // black — fully visited
		return false
	}

	for node := range graph {
		if color[node] == 0 {
			cyclePath = nil
			if dfs(node) {
				diags = append(diags, CompileError{
					Severity: SeverityError,
					Category: "cycle",
					Message:  fmt.Sprintf("Circular dependency detected in blast radius graph: %s", strings.Join(cyclePath, " → ")),
				})
				break // one cycle error is enough
			}
		}
	}

	return diags, graph, isolationSet
}

// ReachableFrom returns all services reachable from `start` in the blast graph.
// Used by the `dg graph` CLI command.
func ReachableFrom(graph map[string][]string, start string) []string {
	visited := make(map[string]bool)
	var order []string
	var walk func(node string)
	walk = func(node string) {
		if visited[node] {
			return
		}
		visited[node] = true
		order = append(order, node)
		for _, n := range graph[node] {
			walk(n)
		}
	}
	walk(start)
	return order
}
