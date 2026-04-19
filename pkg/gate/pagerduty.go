package gate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const pdEventsV2URL = "https://events.pagerduty.com/v2/enqueue"

// pdEvent is the PagerDuty Events API v2 payload.
type pdEvent struct {
	RoutingKey  string     `json:"routing_key"`
	EventAction string     `json:"event_action"` // "trigger" or "resolve"
	DedupKey    string     `json:"dedup_key"`
	Payload     *pdPayload `json:"payload,omitempty"`
}

type pdPayload struct {
	Summary       string                 `json:"summary"`
	Severity      string                 `json:"severity"` // "critical", "error", "warning", "info"
	Source        string                 `json:"source"`
	CustomDetails map[string]interface{} `json:"custom_details,omitempty"`
}

// pdDedupKey returns a stable key so repeated fires don't open duplicate incidents.
func pdDedupKey(svc, tier string) string {
	return fmt.Sprintf("iddc-%s-%s", svc, tier)
}

// sendPagerDuty triggers a PagerDuty incident via the Events API v2.
func sendPagerDuty(routingKey, svc, tier string, signals map[string]float64) error {
	details := map[string]interface{}{
		"service": svc,
		"tier":    tier,
	}
	for k, v := range signals {
		details["signal_"+k] = v
	}

	event := pdEvent{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		DedupKey:    pdDedupKey(svc, tier),
		Payload: &pdPayload{
			Summary:       fmt.Sprintf("IDDC Gate Fired: %s entering %s tier", svc, tier),
			Severity:      "critical",
			Source:        "iddc-engine",
			CustomDetails: details,
		},
	}
	return postPDEvent(event)
}

// resolvePagerDuty resolves the open PagerDuty incident when the gate closes.
func resolvePagerDuty(routingKey, svc, tier string) error {
	event := pdEvent{
		RoutingKey:  routingKey,
		EventAction: "resolve",
		DedupKey:    pdDedupKey(svc, tier),
	}
	return postPDEvent(event)
}

func postPDEvent(event pdEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("pagerduty: marshal: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	backoff := 500 * time.Millisecond
	for attempt := 0; attempt <= 3; attempt++ {
		resp, err := client.Post(pdEventsV2URL, "application/json", bytes.NewReader(payload))
		if err != nil {
			if attempt < 3 {
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			return fmt.Errorf("pagerduty: POST failed: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("pagerduty: HTTP %d", resp.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("pagerduty: max retries exhausted")
}
