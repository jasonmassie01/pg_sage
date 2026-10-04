package specialist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxNoteRunes bounds a PagerDuty note (PagerDuty's limit is 25,000).
const maxNoteRunes = 20000

const notifyTimeout = 15 * time.Second

// PagerDutyNotifier posts a result as a note on the PagerDuty incident
// (REST API, operator-configured base URL and token). The note carries the
// diagnosis only, never the caller's text.
type PagerDutyNotifier struct {
	BaseURL string
	Token   string
	From    string
	Client  *http.Client
}

// Deliver implements Notifier.
func (n *PagerDutyNotifier) Deliver(ctx context.Context, rec Record, res Result) error {
	if rec.ExternalRef == nil || !pdServiceID.MatchString(rec.ExternalRef.ID) {
		return invalidf("the request has no PagerDuty incident id")
	}
	body, err := json.Marshal(map[string]map[string]string{
		"note": {"content": noteText(res)}})
	if err != nil {
		return fmt.Errorf("encode the PagerDuty note: %w", err)
	}
	endpoint := strings.TrimRight(n.BaseURL, "/") + "/incidents/" +
		url.PathEscape(rec.ExternalRef.ID) + "/notes"
	return post(ctx, n.Client, endpoint, body, map[string]string{
		"Authorization": "Token token=" + n.Token, "From": n.From,
		"Accept": "application/vnd.pagerduty+json;version=2"})
}

// WebhookNotifier posts the signed result JSON to the configured URL.
type WebhookNotifier struct {
	URL    string
	Secret string
	Client *http.Client
	Now    func() time.Time
}

// Deliver implements Notifier.
func (n *WebhookNotifier) Deliver(ctx context.Context, _ Record, res Result) error {
	body, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encode the result: %w", err)
	}
	now := time.Now
	if n.Now != nil {
		now = n.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	return post(ctx, n.Client, n.URL, body, map[string]string{"X-Sage-Timestamp": ts,
		"X-Sage-Signature":        SignWebhook(n.Secret, ts, body),
		"X-Sage-Contract-Version": ContractVersion})
}

func post(ctx context.Context, client *http.Client, endpoint string, body []byte,
	headers map[string]string) error {
	if client == nil {
		client = &http.Client{Timeout: notifyTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the result post: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post the result: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the endpoint answered %d", resp.StatusCode)
	}
	return nil
}

// noteText renders a result for a human responder.
func noteText(r Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pg_sage (%s) investigation %s on %s: %s", r.ContractVersion,
		r.Investigation.ID, r.Database, r.Outcome)
	if r.OutcomeReason != "" {
		fmt.Fprintf(&b, " (%s)", r.OutcomeReason)
	}
	b.WriteString("\n")
	if rc := r.RootCause; rc != nil {
		fmt.Fprintf(&b, "\nRoot cause: %s [%s, %s, %s]\n", rc.Label, rc.Node, rc.Source,
			rc.Authority)
	}
	if c := r.Confidence; c.Score != nil {
		fmt.Fprintf(&b, "Confidence: score %.2f (%s)\n", *c.Score, c.Calibration)
	} else {
		fmt.Fprintf(&b, "Confidence: none (%s)\n", c.Calibration)
	}
	for _, l := range r.CausalChain {
		fmt.Fprintf(&b, "%d. %s: %s\n", l.Ordinal, l.Role, l.Label)
		for _, c := range l.Evidence {
			fmt.Fprintf(&b, "   - %s [%s]\n", c.Text, c.EvidenceID)
		}
	}
	if len(r.MissingEvidence) > 0 {
		b.WriteString("\nMissing evidence:\n")
		for _, m := range r.MissingEvidence {
			fmt.Fprintf(&b, "- %s (%s): %s\n", m.ProbeID, m.Status, m.Reason)
		}
	}
	if len(r.Remediations) > 0 {
		b.WriteString("\nCandidate remediations (request one through the contract; " +
			"pg_sage's gate decides):\n")
		for _, rem := range r.Remediations {
			fmt.Fprintf(&b, "- %s: %s (gate preview %s)\n", rem.ID, rem.Title,
				rem.Gate.Verdict)
		}
	}
	text := b.String()
	if runes := []rune(text); len(runes) > maxNoteRunes {
		text = string(runes[:maxNoteRunes])
	}
	return text
}
