package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/audit"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/compiler"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/enforcer"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/sidecar"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/enforcement/writequeue"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/gate"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/policy"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/runtime"
	"github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/pkg/sigfetch"
)

//go:embed console.html
var consoleHTML []byte

// controlAPI serves the engine's operator surface on the control address:
//
//	GET  /                  web console
//	GET  /api/status        applied tier, proposed tier, lock, gate, flags, signals, queue
//	GET  /api/policy        tier ladder, signal ranges, and a verified example scenario per tier
//	GET  /api/events?n=50   most recent audit events
//	POST /api/validate      body: degradation.yaml → diagnostics
//	POST /api/simulate      body: {"yaml": "...optional...", "signals": {"rps": 2500}} → matched tier
//	POST /inject            body: {"rps": 2500}            — set injected signal values
//	GET  /signals                                          — injected signal values
//	POST /write             body: {"id":"w1","payload":""} — buffer a write (queue tiers only)
//	GET  /write/depth                                      — queue depth + drop count
//	POST /override          body: {"tier":"nominal","duration_min":10} — force + lock a tier
//	POST /override/release                                 — release the lock
//	*    /gate/...                                         — same routes as the gate callback server
type controlAPI struct {
	bundle     *compiler.CompiledBundle
	engine     *runtime.Engine
	enforcer   *enforcer.Enforcer
	store      *sigfetch.InjectedStore
	injectable map[string]bool
	queue      *writequeue.Queue
	sidecar    *sidecar.Server
	gate       *gate.HumanGate
	gateRoutes http.Handler
	audit      *audit.Logger
}

func (c *controlAPI) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", c.handleConsole)
	mux.HandleFunc("/api/status", c.handleStatus)
	mux.HandleFunc("/api/policy", c.handlePolicy)
	mux.HandleFunc("/api/events", c.handleEvents)
	mux.HandleFunc("/api/validate", c.handleValidate)
	mux.HandleFunc("/api/simulate", c.handleSimulate)
	mux.HandleFunc("/inject", c.handleInject)
	mux.HandleFunc("/signals", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, c.store.Get()) })
	mux.HandleFunc("/write", c.handleWrite)
	mux.HandleFunc("/write/depth", c.handleWriteDepth)
	mux.HandleFunc("/override", c.handleOverride)
	mux.HandleFunc("/override/release", c.handleRelease)
	if c.gateRoutes != nil {
		mux.Handle("/gate/", c.gateRoutes)
	}
	return mux
}

func (c *controlAPI) handleConsole(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}

// ─── status ──────────────────────────────────────────────────────────────────

type lockView struct {
	Tier  string    `json:"tier"`
	Until time.Time `json:"until"`
}

type queueView struct {
	Enabled bool  `json:"enabled"`
	Active  bool  `json:"active"`
	Depth   int   `json:"depth"`
	Max     int   `json:"max"`
	Dropped int64 `json:"dropped"`
}

type statusView struct {
	Service    string               `json:"service"`
	Version    string               `json:"version"`
	Applied    enforcer.State       `json:"applied"`
	Proposed   string               `json:"proposed"` // tier the evaluator wants right now
	Lock       *lockView            `json:"lock"`
	Gate       gate.Status          `json:"gate"`
	Flags      sidecar.FeatureFlags `json:"flags"`
	Signals    map[string]float64   `json:"signals"`
	Stale      map[string]bool      `json:"stale"`
	Injectable map[string]bool      `json:"injectable"`
	Queue      queueView            `json:"queue"`
	Now        time.Time            `json:"now"`
}

func (c *controlAPI) status() statusView {
	snap := c.engine.Bus().Snapshot()
	st := statusView{
		Service:    c.bundle.Service,
		Version:    c.bundle.Version,
		Applied:    c.enforcer.State(),
		Proposed:   c.engine.CurrentTier(),
		Gate:       c.gate.Status(),
		Flags:      c.sidecar.Features(),
		Signals:    snap.Values,
		Stale:      snap.Stale,
		Injectable: c.injectable,
		Now:        time.Now(),
	}
	if tier, until, ok := c.engine.LockState(); ok {
		st.Lock = &lockView{Tier: tier, Until: until}
	}
	if c.queue != nil {
		st.Queue = queueView{
			Enabled: true,
			Active:  st.Applied.QueueActive,
			Depth:   c.queue.Depth(),
			Max:     c.queue.MaxDepth(),
			Dropped: c.queue.Dropped(),
		}
	}
	return st
}

func (c *controlAPI) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, c.status())
}

func (c *controlAPI) handleEvents(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 50
	}
	writeJSON(w, c.audit.Recent(n))
}

// ─── policy view ─────────────────────────────────────────────────────────────

type tierView struct {
	Name     string             `json:"name"`
	Index    int                `json:"index"`
	Mode     string             `json:"mode"`
	When     string             `json:"when"`
	Disable  []string           `json:"disable"`
	Queue    *queueSpecView     `json:"queue_writes,omitempty"`
	Gate     *gateView          `json:"gate,omitempty"`
	Scenario map[string]float64 `json:"scenario,omitempty"` // signal values that land exactly on this tier
	Features map[string]bool    `json:"features"`
}

type queueSpecView struct {
	MaxDepth int    `json:"max_depth"`
	TTL      string `json:"ttl"`
}

type gateView struct {
	Wait      string   `json:"wait"`
	OnTimeout string   `json:"on_timeout"`
	Notify    []string `json:"notify,omitempty"`
}

type signalView struct {
	ID         string  `json:"id"`
	Source     string  `json:"source"`
	Injectable bool    `json:"injectable"`
	Max        float64 `json:"max"`
	Step       float64 `json:"step"`
}

type policyView struct {
	Service   string       `json:"service"`
	Version   string       `json:"version"`
	Checksum  string       `json:"checksum"`
	Compiled  time.Time    `json:"compiled_at"`
	Tiers     []tierView   `json:"tiers"`
	Signals   []signalView `json:"signals"`
	DependsOn []string     `json:"depends_on"`
}

func (c *controlAPI) handlePolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, describeBundle(c.bundle, c.injectable))
}

func describeBundle(b *compiler.CompiledBundle, injectable map[string]bool) policyView {
	table := sidecar.PolicyFromBundle(b)
	v := policyView{
		Service:   b.Service,
		Version:   b.Version,
		Checksum:  fmt.Sprintf("%x", b.SourceChecksum[:6]),
		Compiled:  b.CompiledAt,
		DependsOn: b.BlastGraph[b.Service],
	}
	for i, t := range b.Tiers {
		tv := tierView{
			Name:     t.Name,
			Index:    i,
			Mode:     table.Tiers[t.Name].Mode,
			When:     describeCondition(t.Condition),
			Disable:  t.Behavior.Disable,
			Scenario: scenarioFor(b, i),
			Features: table.Tiers[t.Name].Features,
		}
		if q := t.Behavior.QueueWrites; q != nil {
			tv.Queue = &queueSpecView{MaxDepth: q.MaxDepth, TTL: q.TTL.String()}
		}
		if tv.Disable == nil {
			tv.Disable = []string{}
		}
		if g := t.GateSpec; g != nil {
			onTimeout := g.OnTimeout
			if onTimeout == "" {
				onTimeout = "execute_policy"
			}
			tv.Gate = &gateView{Wait: g.Wait.String(), OnTimeout: onTimeout, Notify: g.Notify}
		}
		v.Tiers = append(v.Tiers, tv)
	}
	maxima := map[string]float64{}
	for _, t := range b.Tiers {
		collectThresholds(t.Condition, maxima)
	}
	for _, s := range b.SignalSources {
		max, step := sliderRange(maxima[s.ID])
		v.Signals = append(v.Signals, signalView{
			ID: s.ID, Source: s.Source, Injectable: injectable[s.ID], Max: max, Step: step,
		})
	}
	return v
}

// describeCondition renders a condition tree as a short human-readable string.
func describeCondition(c policy.Condition) string {
	var parts []string
	ids := make([]string, 0, len(c.Comparisons))
	for id := range c.Comparisons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		parts = append(parts, describeComparator(id, c.Comparisons[id]))
	}
	for _, sub := range c.AllOf {
		parts = append(parts, wrap(describeCondition(sub), len(c.AllOf) > 1))
	}
	if len(c.AnyOf) > 0 {
		var alts []string
		for _, sub := range c.AnyOf {
			alts = append(alts, wrap(describeCondition(sub), len(c.AnyOf) > 1))
		}
		parts = append(parts, strings.Join(alts, " OR "))
	}
	if len(parts) == 0 {
		return "always"
	}
	return strings.Join(parts, " AND ")
}

func wrap(s string, multi bool) string {
	if multi && strings.ContainsAny(s, " ") && (strings.Contains(s, " AND ") || strings.Contains(s, " OR ")) {
		return "(" + s + ")"
	}
	return s
}

func describeComparator(id string, c policy.Comparator) string {
	var bounds []string
	add := func(op string, v *float64) {
		if v != nil {
			bounds = append(bounds, op+" "+fmtNum(*v))
		}
	}
	add("=", c.Eq)
	add("≥", c.Gte)
	add(">", c.Gt)
	add("<", c.Lt)
	add("≤", c.Lte)
	return id + " " + strings.Join(bounds, " and ")
}

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func collectThresholds(c policy.Condition, max map[string]float64) {
	for id, cmp := range c.Comparisons {
		for _, v := range []*float64{cmp.Eq, cmp.Gte, cmp.Gt, cmp.Lt, cmp.Lte} {
			if v != nil && *v > max[id] {
				max[id] = *v
			}
		}
	}
	for _, sub := range c.AllOf {
		collectThresholds(sub, max)
	}
	for _, sub := range c.AnyOf {
		collectThresholds(sub, max)
	}
}

// sliderRange picks a console slider range comfortably above the highest threshold.
func sliderRange(threshold float64) (max, step float64) {
	switch {
	case threshold <= 0:
		return 100, 1
	case threshold <= 1:
		return 1, 0.01
	default:
		max = threshold * 1.25
		return max, max / 500
	}
}

// scenarioFor builds signal values (all other signals at 0) that make the
// evaluator pick exactly tier idx, or nil if no simple witness is found.
func scenarioFor(b *compiler.CompiledBundle, idx int) map[string]float64 {
	want := b.Tiers[idx].Name
	candidates := []policy.Condition{b.Tiers[idx].Condition}
	if alts := b.Tiers[idx].Condition.AnyOf; len(alts) > 0 {
		candidates = alts
	}
	for _, cond := range candidates {
		vals := make(map[string]float64, len(b.SignalSources))
		for _, s := range b.SignalSources {
			vals[s.ID] = 0
		}
		witness(cond, vals)
		if runtime.MatchTier(b, vals) == want {
			return vals
		}
	}
	return nil
}

// witness writes values that satisfy c into vals (first branch of any_of).
func witness(c policy.Condition, vals map[string]float64) {
	for id, cmp := range c.Comparisons {
		vals[id] = pickValue(cmp)
	}
	for _, sub := range c.AllOf {
		witness(sub, vals)
	}
	if len(c.AnyOf) > 0 {
		witness(c.AnyOf[0], vals)
	}
}

func pickValue(c policy.Comparator) float64 {
	if c.Eq != nil {
		return *c.Eq
	}
	var lo, hi *float64
	loIncl := false
	if c.Gte != nil {
		lo, loIncl = c.Gte, true
	} else if c.Gt != nil {
		lo = c.Gt
	}
	if c.Lt != nil {
		hi = c.Lt
	} else if c.Lte != nil {
		hi = c.Lte
	}
	switch {
	case lo != nil && hi != nil:
		return (*lo + *hi) / 2
	case lo != nil && loIncl:
		return *lo
	case lo != nil:
		if *lo == 0 {
			return 1
		}
		return *lo * 1.1
	case hi != nil && *hi > 0:
		return *hi / 2
	case hi != nil:
		return *hi - 1
	}
	return 0
}

// ─── validate / simulate ─────────────────────────────────────────────────────

type diagView struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

type validateView struct {
	Valid       bool        `json:"valid"`
	ParseError  string      `json:"parse_error,omitempty"`
	Diagnostics []diagView  `json:"diagnostics"`
	Policy      *policyView `json:"policy,omitempty"`
}

// maxPolicyBytes bounds request bodies for validate/simulate.
const maxPolicyBytes = 1 << 20

func compileSource(src []byte) (validateView, *compiler.CompiledBundle) {
	v := validateView{Diagnostics: []diagView{}}
	p, err := policy.LoadFromBytes(src)
	if err != nil {
		v.ParseError = err.Error()
		return v, nil
	}
	result, err := compiler.Compile(p, src)
	for _, d := range result.Diagnostics {
		v.Diagnostics = append(v.Diagnostics, diagView{string(d.Severity), d.Category, d.Message})
	}
	if err != nil {
		return v, nil
	}
	v.Valid = true
	pv := describeBundle(result.Bundle, nil)
	v.Policy = &pv
	return v, result.Bundle
}

func (c *controlAPI) handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	src, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, _ := compileSource(src)
	writeJSON(w, v)
}

func (c *controlAPI) handleSimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		YAML    string             `json:"yaml"`
		Signals map[string]float64 `json:"signals"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPolicyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	bundle := c.bundle
	resp := map[string]interface{}{}
	if strings.TrimSpace(req.YAML) != "" {
		v, b := compileSource([]byte(req.YAML))
		resp["validation"] = v
		if b == nil {
			writeJSON(w, resp)
			return
		}
		bundle = b
	}
	tier := runtime.MatchTier(bundle, req.Signals)
	table := sidecar.PolicyFromBundle(bundle)
	resp["tier"] = tier
	resp["mode"] = table.Tiers[tier].Mode
	resp["features"] = table.Tiers[tier].Features
	resp["signals"] = req.Signals
	writeJSON(w, resp)
}

// ─── signals, queue, overrides ───────────────────────────────────────────────

func (c *controlAPI) handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var vals map[string]float64
	if err := json.NewDecoder(r.Body).Decode(&vals); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	var rejected []string
	for id := range vals {
		if !c.injectable[id] {
			rejected = append(rejected, id)
		}
	}
	if len(rejected) > 0 {
		sort.Strings(rejected)
		http.Error(w, fmt.Sprintf("not injectable (undeclared, or collected from a live source): %s",
			strings.Join(rejected, ", ")), http.StatusBadRequest)
		return
	}
	c.store.Set(vals)

	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("[inject]  ")
	for _, k := range keys {
		fmt.Printf("%s=%.2f  ", k, vals[k])
	}
	fmt.Println()

	writeJSON(w, map[string]interface{}{
		"injected": vals,
		"ts":       time.Now().Format(time.RFC3339),
	})
}

func (c *controlAPI) handleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if c.queue == nil {
		http.Error(w, "no tier in this policy declares queue_writes", http.StatusNotFound)
		return
	}
	if !c.enforcer.QueueActive() {
		http.Error(w, fmt.Sprintf("write queue inactive in tier %q — write to the backend directly",
			c.enforcer.State().Tier), http.StatusConflict)
		return
	}
	var req struct {
		ID      string `json:"id"`
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := c.queue.Enqueue(writequeue.Write{ID: req.ID, Payload: []byte(req.Payload)}); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]interface{}{"queued": req.ID, "depth": c.queue.Depth()})
}

func (c *controlAPI) handleWriteDepth(w http.ResponseWriter, r *http.Request) {
	if c.queue == nil {
		http.Error(w, "no tier in this policy declares queue_writes", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]interface{}{
		"depth":   c.queue.Depth(),
		"dropped": c.queue.Dropped(),
		"active":  c.enforcer.QueueActive(),
	})
}

func (c *controlAPI) handleOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Tier        string `json:"tier"`
		DurationMin int    `json:"duration_min"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.DurationMin <= 0 {
		req.DurationMin = 10
	}
	dur := time.Duration(req.DurationMin) * time.Minute
	if err := c.enforcer.Force(req.Tier, dur, "manual override"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]interface{}{
		"locked_tier":  req.Tier,
		"duration_min": req.DurationMin,
		"locked_until": time.Now().Add(dur).Format(time.RFC3339),
	})
}

func (c *controlAPI) handleRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	c.enforcer.Release()
	writeJSON(w, map[string]string{"status": "released"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
