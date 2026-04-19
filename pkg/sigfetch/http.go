package sigfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// NewHTTPFetcher returns a Fetcher that GETs url and extracts a float64 from
// the JSON response. field is the top-level JSON key to read (e.g. "value").
// If field is empty, the response body must be a bare JSON number.
//
// Example: NewHTTPFetcher("http://app:8080/metrics/rps", "value")
// expects: {"value": 1234.5, ...}
func NewHTTPFetcher(url, field string) Fetcher {
	client := &http.Client{Timeout: 5 * time.Second}
	return func(ctx context.Context) (float64, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, fmt.Errorf("http fetcher build request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, fmt.Errorf("http fetcher GET %s: %w", url, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("http fetcher %s: HTTP %d", url, resp.StatusCode)
		}

		if field == "" {
			var v float64
			if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
				return 0, fmt.Errorf("http fetcher decode float: %w", err)
			}
			return v, nil
		}

		var result map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return 0, fmt.Errorf("http fetcher decode JSON: %w", err)
		}
		raw, ok := result[field]
		if !ok {
			return 0, fmt.Errorf("http fetcher: field %q not in response from %s", field, url)
		}
		switch v := raw.(type) {
		case float64:
			return v, nil
		case string:
			parsed, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return 0, fmt.Errorf("http fetcher: field %q = %q: %w", field, v, err)
			}
			return parsed, nil
		default:
			return 0, fmt.Errorf("http fetcher: field %q has type %T, want number", field, raw)
		}
	}
}
