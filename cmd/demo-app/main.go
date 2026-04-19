// Command demo-app is a tiny e-commerce simulation that honours IDDC tier signals.
//
// It queries the sidecar server every request to get the current feature flags
// and adjusts its behaviour accordingly:
//
//	nominal   — all features enabled (recommendations, search, checkout)
//	warm      — recommendations disabled (read-heavy features off)
//	hot       — uploads disabled as well; static product list served
//	critical  — search returns cached results only; checkout queued
//	survival  — static 503 fallback page for all routes
//
// Usage:
//
//	./demo-app [--addr :8080] [--sidecar http://localhost:8081]
//
// Endpoints:
//
//	GET /                   — homepage with feature status
//	GET /recommendations    — personalised product list (off in warm+)
//	GET /search?q=...       — product search (cached in critical)
//	POST /checkout          — place order (queued in critical)
//	GET /health             — health check
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"iddc/pkg/enforcement/fallback"
)

// sidecarFlags is fetched from the sidecar /flags endpoint.
type sidecarFlags struct {
	Tier     string          `json:"tier"`
	Features map[string]bool `json:"features"`
}

func fetchFlags(sidecarURL string) (*sidecarFlags, error) {
	resp, err := http.Get(sidecarURL + "/flags")
	if err != nil {
		// If sidecar is unreachable, degrade to nominal (fail-open).
		return &sidecarFlags{
			Tier:     "nominal",
			Features: map[string]bool{"recommendations": true, "analytics": true, "uploads": true},
		}, nil
	}
	defer resp.Body.Close()
	var f sidecarFlags
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

func run(addr, sidecarURL, engineURL string) error {
	mux := http.NewServeMux()

	// ── / — homepage ──────────────────────────────────────────────────────────
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		flags, err := fetchFlags(sidecarURL)
		if err != nil {
			http.Error(w, "sidecar error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if flags.Tier == "survival" {
			serveFallback(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, homePage(flags))
	})

	// ── /recommendations ──────────────────────────────────────────────────────
	mux.HandleFunc("/recommendations", func(w http.ResponseWriter, r *http.Request) {
		flags, _ := fetchFlags(sidecarURL)

		if flags.Tier == "survival" {
			serveFallback(w, r)
			return
		}
		if !flags.Features["recommendations"] {
			writeJSON(w, map[string]interface{}{
				"tier":    flags.Tier,
				"status":  "disabled",
				"message": "Personalised recommendations are temporarily unavailable.",
				"items":   []string{},
			})
			return
		}
		writeJSON(w, map[string]interface{}{
			"tier":   flags.Tier,
			"status": "ok",
			"items": []string{
				"Wireless Headphones — $79",
				"Mechanical Keyboard — $149",
				"USB-C Hub — $39",
			},
		})
	})

	// ── /search ───────────────────────────────────────────────────────────────
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		flags, _ := fetchFlags(sidecarURL)
		q := r.URL.Query().Get("q")

		if flags.Tier == "survival" {
			serveFallback(w, r)
			return
		}

		cached := flags.Tier == "critical" || flags.Tier == "survival"
		writeJSON(w, map[string]interface{}{
			"tier":   flags.Tier,
			"query":  q,
			"cached": cached,
			"results": filterResults(q, cached),
		})
	})

	// ── /checkout ─────────────────────────────────────────────────────────────
	mux.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		flags, _ := fetchFlags(sidecarURL)

		if flags.Tier == "survival" {
			serveFallback(w, r)
			return
		}

		if flags.Tier == "critical" {
			// Simulate write queue — in a real app, push to POST :9191/write
			w.WriteHeader(http.StatusAccepted)
			writeJSON(w, map[string]interface{}{
				"tier":    flags.Tier,
				"status":  "queued",
				"message": "Your order has been received and will be processed shortly.",
				"ref":     fmt.Sprintf("ORD-%d", time.Now().UnixMilli()),
			})
			return
		}

		writeJSON(w, map[string]interface{}{
			"tier":    flags.Tier,
			"status":  "confirmed",
			"message": "Order placed successfully!",
			"ref":     fmt.Sprintf("ORD-%d", time.Now().UnixMilli()),
		})
	})

	// ── /health ───────────────────────────────────────────────────────────────
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		flags, _ := fetchFlags(sidecarURL)
		writeJSON(w, map[string]interface{}{
			"status": "ok",
			"tier":   flags.Tier,
			"ts":     time.Now().Format(time.RFC3339),
		})
	})

	// ── /inject — proxy to dg-engine (enables browser-based signal injection) ──
	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		resp, err := http.Post(engineURL+"/inject", "application/json", r.Body)
		if err != nil {
			http.Error(w, "engine unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	fmt.Printf("demo-app  listening on %s\n", addr)
	fmt.Printf("sidecar   polling   %s/flags\n", sidecarURL)
	fmt.Printf("engine    inject    %s/inject\n\n", engineURL)
	return http.ListenAndServe(addr, mux)
}

// ─── response helpers ─────────────────────────────────────────────────────────

func serveFallback(w http.ResponseWriter, r *http.Request) {
	fallback.Handler().ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func filterResults(q string, cached bool) []map[string]string {
	catalog := []map[string]string{
		{"name": "Wireless Headphones", "price": "$79"},
		{"name": "Mechanical Keyboard", "price": "$149"},
		{"name": "USB-C Hub", "price": "$39"},
		{"name": "Laptop Stand", "price": "$55"},
		{"name": "Webcam HD", "price": "$89"},
	}
	if cached {
		// Return first 2 items as "cached" results — no live DB query.
		return catalog[:2]
	}
	if q == "" {
		return catalog
	}
	q = strings.ToLower(q)
	var out []map[string]string
	for _, item := range catalog {
		if strings.Contains(strings.ToLower(item["name"]), q) {
			out = append(out, item)
		}
	}
	if out == nil {
		return []map[string]string{}
	}
	return out
}

func homePage(flags *sidecarFlags) string {
	tier := flags.Tier
	tierColor := map[string]string{
		"nominal":  "#27ae60",
		"warm":     "#f39c12",
		"hot":      "#e67e22",
		"critical": "#e74c3c",
		"survival": "#c0392b",
	}
	color := tierColor[tier]
	if color == "" {
		color = "#888"
	}

	featureRow := func(name string, enabled bool) string {
		icon := "✓"
		c := "#27ae60"
		if !enabled {
			icon = "✗"
			c = "#e74c3c"
		}
		return fmt.Sprintf(`<tr><td>%s</td><td style="color:%s;font-weight:bold">%s %v</td></tr>`, name, c, icon, enabled)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>IDDC Demo</title>
  <meta http-equiv="refresh" content="3">
  <style>
    *{box-sizing:border-box}
    body{font-family:system-ui,sans-serif;max-width:680px;margin:4vh auto;padding:0 1rem;color:#333}
    h1{margin-bottom:0;font-size:1.6rem}
    .sub{color:#888;font-size:.85rem;margin-bottom:1.2rem}
    .tier{display:inline-block;background:%s;color:#fff;padding:4px 16px;border-radius:20px;font-size:1.1em;font-weight:bold;letter-spacing:.03em}
    table{border-collapse:collapse;width:100%%;margin-top:.8rem}
    td,th{padding:7px 12px;border-bottom:1px solid #eee;text-align:left}
    .ok{color:#27ae60;font-weight:bold} .no{color:#e74c3c;font-weight:bold}
    .panel{margin-top:1.6rem;background:#f8f9fa;border:1px solid #dee2e6;border-radius:8px;padding:1rem}
    .panel h3{margin:0 0 .8rem;font-size:1rem;color:#555}
    .scenarios{display:grid;grid-template-columns:repeat(auto-fill,minmax(140px,1fr));gap:.5rem}
    .btn{border:none;border-radius:6px;padding:8px 4px;font-size:.85rem;font-weight:600;cursor:pointer;width:100%%;transition:opacity .15s}
    .btn:hover{opacity:.85}
    .btn-nominal{background:#27ae60;color:#fff}
    .btn-warm{background:#f39c12;color:#fff}
    .btn-hot{background:#e67e22;color:#fff}
    .btn-critical{background:#e74c3c;color:#fff}
    .btn-survival{background:#c0392b;color:#fff}
    .btn-recover{background:#2ecc71;color:#fff}
    #status{margin-top:.6rem;font-size:.8rem;color:#666;min-height:1.2em}
    .links{margin-top:1rem;font-size:.85rem}
    .links a{color:#3498db;margin-right:1rem}
  </style>
</head>
<body>
  <h1>IDDC Demo</h1>
  <p class="sub">Intent-Driven Degradation Contracts — live tier simulation</p>
  <p>Active tier: <span class="tier">%s</span></p>
  <table>
    <tr><th>Feature</th><th>Status</th></tr>
    %s
    %s
    %s
  </table>

  <div class="panel">
    <h3>Simulate load — click to inject signals</h3>
    <div class="scenarios">
      <button class="btn btn-nominal"  onclick="inject({rps:100})">Nominal<br><small>rps=100</small></button>
      <button class="btn btn-warm"     onclick="inject({rps:900})">Warm<br><small>rps=900</small></button>
      <button class="btn btn-hot"      onclick="inject({rps:1600,pod_ceiling:0.90})">Hot<br><small>rps=1600</small></button>
      <button class="btn btn-critical" onclick="inject({rps:2500})">Critical<br><small>rps=2500</small></button>
      <button class="btn btn-survival" onclick="inject({mem_pressure:0.96})">Survival<br><small>mem=96%%</small></button>
      <button class="btn btn-recover"  onclick="inject({rps:50,pod_ceiling:0.2,mem_pressure:0.1})">Recover<br><small>all low</small></button>
    </div>
    <div id="status"></div>
  </div>

  <div class="links">
    <a href="/search?q=keyboard">Search</a>
    <a href="/recommendations">Recommendations</a>
    <a href="/health">Health</a>
  </div>
  <p style="font-size:.75rem;color:#aaa;margin-top:1rem">Auto-refreshes every 3s &nbsp;·&nbsp; tier transitions take ~3–5s (dev hysteresis)</p>

  <script>
  async function inject(signals) {
    const el = document.getElementById('status');
    el.textContent = 'Injecting…';
    try {
      const r = await fetch('/inject', {
        method: 'POST',
        headers: {'Content-Type':'application/json'},
        body: JSON.stringify(signals)
      });
      const d = await r.json();
      el.textContent = 'Injected: ' + JSON.stringify(d.injected);
    } catch(e) {
      el.textContent = 'Error: ' + e.message;
    }
  }
  </script>
</body>
</html>`,
		color, tier,
		featureRow("recommendations", flags.Features["recommendations"]),
		featureRow("analytics", flags.Features["analytics"]),
		featureRow("uploads", flags.Features["uploads"]),
	)
}

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	var addr string
	var sidecarURL string
	var engineURL string

	root := &cobra.Command{
		Use:   "demo-app",
		Short: "IDDC demo e-commerce app — honours degradation tier flags",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(addr, sidecarURL, engineURL)
		},
	}

	root.Flags().StringVar(&addr, "addr", ":8080", "Listen address for the demo app")
	root.Flags().StringVar(&sidecarURL, "sidecar", "http://localhost:8081", "Sidecar base URL")
	root.Flags().StringVar(&engineURL, "engine", "http://localhost:9191", "Engine inject base URL")

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
