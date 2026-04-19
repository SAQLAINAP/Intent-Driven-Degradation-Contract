// Package sigfetch provides SignalFetcher implementations for the SignalBus.
//
// Each fetcher is a func(ctx) (float64, error) that the bus calls on its tick.
// This package provides:
//   - PrometheusFetcher  — queries a Prometheus HTTP API
//   - InjectedFetcher    — returns a value from an in-memory map (for tests + simulation)
package sigfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Fetcher is the function signature the SignalBus expects.
type Fetcher func(ctx context.Context) (float64, error)

// ─── Prometheus ───────────────────────────────────────────────────────────────

// prometheusInstantResponse is the JSON shape returned by /api/v1/query.
type prometheusInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]interface{}    `json:"value"` // [timestamp, "value_string"]
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

// NewPrometheusFetcher returns a Fetcher that queries Prometheus for the given
// PromQL expression and returns the first scalar result.
//
// baseURL is the Prometheus base URL, e.g. "http://prometheus:9090".
// query is the PromQL expression, e.g. `rate(http_requests_total[1m])`.
func NewPrometheusFetcher(baseURL, query string) Fetcher {
	client := &http.Client{Timeout: 5 * time.Second}

	return func(ctx context.Context) (float64, error) {
		endpoint := baseURL + "/api/v1/query"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return 0, fmt.Errorf("building prometheus request: %w", err)
		}
		q := url.Values{}
		q.Set("query", query)
		req.URL.RawQuery = q.Encode()

		resp, err := client.Do(req)
		if err != nil {
			return 0, fmt.Errorf("prometheus query failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("prometheus returned HTTP %d", resp.StatusCode)
		}

		var pr prometheusInstantResponse
		if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
			return 0, fmt.Errorf("decoding prometheus response: %w", err)
		}
		if pr.Status != "success" {
			return 0, fmt.Errorf("prometheus error: %s", pr.Error)
		}
		if len(pr.Data.Result) == 0 {
			return 0, fmt.Errorf("prometheus query returned no results for: %s", query)
		}

		// Value is [timestamp, "value_as_string"].
		valStr, ok := pr.Data.Result[0].Value[1].(string)
		if !ok {
			return 0, fmt.Errorf("unexpected prometheus value type")
		}
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing prometheus value %q: %w", valStr, err)
		}
		return v, nil
	}
}

// ─── Injected (test / simulate) ───────────────────────────────────────────────

// InjectedStore is a thread-safe map of signal ID → value.
// The engine's /inject HTTP endpoint writes into this; fetchers read from it.
type InjectedStore struct {
	ch chan map[string]float64 // single-element channel acts as a mutex-free store
}

// NewInjectedStore creates an empty store.
func NewInjectedStore() *InjectedStore {
	s := &InjectedStore{ch: make(chan map[string]float64, 1)}
	s.ch <- map[string]float64{} // seed with empty map
	return s
}

// Set updates one or more signal values atomically.
func (s *InjectedStore) Set(vals map[string]float64) {
	old := <-s.ch
	merged := make(map[string]float64, len(old)+len(vals))
	for k, v := range old {
		merged[k] = v
	}
	for k, v := range vals {
		merged[k] = v
	}
	s.ch <- merged
}

// Get returns a snapshot of all current values.
func (s *InjectedStore) Get() map[string]float64 {
	vals := <-s.ch
	s.ch <- vals
	copy := make(map[string]float64, len(vals))
	for k, v := range vals {
		copy[k] = v
	}
	return copy
}

// FetcherFor returns a Fetcher for a specific signal ID backed by this store.
func (s *InjectedStore) FetcherFor(signalID string) Fetcher {
	return func(ctx context.Context) (float64, error) {
		vals := s.Get()
		v, ok := vals[signalID]
		if !ok {
			return 0, fmt.Errorf("no injected value for signal %q", signalID)
		}
		return v, nil
	}
}
