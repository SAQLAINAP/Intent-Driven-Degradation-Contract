// Package fallback serves a static HTML page when the system is in survival tier.
package fallback

import (
	"net/http"
)

const staticPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Service Temporarily Degraded</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 480px; margin: 10vh auto; text-align: center; color: #333; }
    h1 { color: #c0392b; }
    p  { color: #555; }
  </style>
</head>
<body>
  <h1>We'll be right back</h1>
  <p>This service is temporarily operating in reduced capacity mode.</p>
  <p>Our team has been notified and is working to restore full service.</p>
</body>
</html>`

// Handler returns an http.Handler that serves the static fallback page for all routes.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(staticPage))
	})
}
