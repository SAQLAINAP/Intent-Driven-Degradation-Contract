// Package gate implements the human-in-the-loop dead man's switch.
package gate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// slackMessage is a minimal Slack Block Kit payload for gate notifications.
type slackMessage struct {
	Blocks []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type string      `json:"type"`
	Text *slackText  `json:"text,omitempty"`
	Fields []slackText `json:"fields,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// buildSlackMessage constructs the Block Kit message for a gate fire event.
func buildSlackMessage(svc, tier string, signals map[string]float64, wait time.Duration, callbackBase string) slackMessage {
	sigText := ""
	for k, v := range signals {
		sigText += fmt.Sprintf("*%s*: %.2f\n", k, v)
	}
	if sigText == "" {
		sigText = "_no signal data_"
	}

	approveURL := fmt.Sprintf("%s/gate/approve", callbackBase)
	overrideURL := fmt.Sprintf("%s/gate/override", callbackBase)
	escalateURL := fmt.Sprintf("%s/gate/escalate", callbackBase)

	return slackMessage{
		Blocks: []slackBlock{
			{
				Type: "header",
				Text: &slackText{Type: "plain_text", Text: fmt.Sprintf("⚠️  IDDC Gate Fired — %s → %s", svc, tier)},
			},
			{
				Type: "section",
				Text: &slackText{
					Type: "mrkdwn",
					Text: fmt.Sprintf("*Service:* %s\n*Target Tier:* `%s`\n*Auto-executes in:* %s\n\n*Signals:*\n%s",
						svc, tier, wait, sigText),
				},
			},
			{
				Type: "section",
				Fields: []slackText{
					{Type: "mrkdwn", Text: fmt.Sprintf("*Approve:*\n`POST %s`", approveURL)},
					{Type: "mrkdwn", Text: fmt.Sprintf("*Override:*\n`POST %s`", overrideURL)},
					{Type: "mrkdwn", Text: fmt.Sprintf("*Escalate:*\n`POST %s`", escalateURL)},
				},
			},
		},
	}
}

// sendSlack posts the message to the given Slack webhook URL.
// Retries up to maxRetries times with exponential backoff on 429 / transient errors.
func sendSlack(webhookURL string, msg slackMessage, maxRetries int) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshalling slack message: %w", err)
	}

	backoff := 500 * time.Millisecond
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
		if err != nil {
			if attempt < maxRetries {
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			return fmt.Errorf("slack webhook POST failed: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("slack webhook: max retries exhausted")
}
