package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"
)

// TeamsSender posts an Adaptive Card to a Microsoft Teams Workflows
// incoming webhook (market-14; Office 365 connectors are retired). The
// channel's webhook_url is a secret and a TargetPolicy target.
type TeamsSender struct {
	client *http.Client
	policy TargetPolicy
}

// NewTeamsSender creates a TeamsSender that refuses internal targets.
func NewTeamsSender() *TeamsSender {
	return NewTeamsSenderWithPolicy(TargetPolicy{})
}

// NewTeamsSenderWithPolicy creates a TeamsSender under policy.
func NewTeamsSenderWithPolicy(policy TargetPolicy) *TeamsSender {
	return &TeamsSender{client: policy.HTTPClient(10 * time.Second), policy: policy}
}

// Type returns the channel type identifier.
func (s *TeamsSender) Type() string { return "teams" }

// teamsMaxPayloadBytes keeps a message under Teams' 28 KB limit; a larger
// card is rejected, so the page would never arrive.
const teamsMaxPayloadBytes = 27 * 1024

// teamsTruncatedNote ends a body that was cut to fit the card.
const teamsTruncatedNote = "… (truncated; see pg_sage for the full text)"

type teamsMessage struct {
	Type        string            `json:"type"`
	Attachments []teamsAttachment `json:"attachments"`
}

type teamsAttachment struct {
	ContentType string            `json:"contentType"`
	Content     teamsAdaptiveCard `json:"content"`
}

type teamsAdaptiveCard struct {
	Schema  string        `json:"$schema"`
	Type    string        `json:"type"`
	Version string        `json:"version"`
	Body    []teamsBlock  `json:"body"`
	Actions []teamsAction `json:"actions,omitempty"`
}

type teamsBlock struct {
	Type   string      `json:"type"`
	Text   string      `json:"text,omitempty"`
	Weight string      `json:"weight,omitempty"`
	Size   string      `json:"size,omitempty"`
	Wrap   bool        `json:"wrap,omitempty"`
	Color  string      `json:"color,omitempty"`
	Facts  []teamsFact `json:"facts,omitempty"`
}

type teamsFact struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

type teamsAction struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Send posts evt as an Adaptive Card to the channel's webhook_url.
func (s *TeamsSender) Send(ctx context.Context, ch Channel, evt Event) error {
	url := ch.Config["webhook_url"]
	if url == "" {
		return fmt.Errorf("teams channel %q: missing webhook_url: %w", ch.Name, ErrPermanent)
	}
	if err := s.policy.ValidateURL(url); err != nil {
		return fmt.Errorf("teams channel %q: %w", ch.Name, err)
	}
	body, err := buildTeamsPayload(ch, evt)
	if err != nil {
		return fmt.Errorf("teams channel %q: %w", ch.Name, err)
	}
	return RedactError(postJSON(ctx, s.client, "teams", url, body, nil))
}

// buildTeamsPayload renders the card and, when it is over the size
// limit, keeps the longest body prefix whose encoded card still fits
// (JSON escaping can grow text sixfold, so the cut measures the card).
func buildTeamsPayload(ch Channel, evt Event) ([]byte, error) {
	whole, err := json.Marshal(teamsMessageFor(ch, evt, evt.Body))
	if err != nil || len(whole) <= teamsMaxPayloadBytes {
		return whole, err
	}
	var best []byte
	lo, hi := 0, len(evt.Body)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		out, err := json.Marshal(teamsMessageFor(ch, evt,
			cutRunes(evt.Body, mid)+teamsTruncatedNote))
		if err != nil {
			return nil, err
		}
		if len(out) <= teamsMaxPayloadBytes {
			best, lo = out, mid+1
		} else {
			hi = mid - 1
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: card is over %d bytes without its body",
			ErrPermanent, teamsMaxPayloadBytes)
	}
	return best, nil
}

// cutRunes returns the longest prefix of s within n bytes that ends on a
// rune boundary.
func cutRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func teamsMessageFor(ch Channel, evt Event, text string) teamsMessage {
	title, color := evt.Subject, teamsColor(evt.Severity)
	if evt.Resolve {
		title, color = "Resolved: "+evt.Subject, "Good"
	}
	facts := []teamsFact{{"Severity", evt.Severity}, {"Event", evt.Type}}
	if db, ok := evt.Data["database"]; ok {
		facts = append(facts, teamsFact{"Database", fmt.Sprintf("%v", db)})
	}
	facts = append(facts, teamsFact{"Dedup key", EventDedupKey(evt)})
	blocks := []teamsBlock{
		{Type: "TextBlock", Text: title, Weight: "Bolder", Size: "Medium", Wrap: true,
			Color: color},
		{Type: "FactSet", Facts: facts},
	}
	if text != "" {
		blocks = append(blocks, teamsBlock{Type: "TextBlock", Text: text, Wrap: true})
	}
	card := teamsAdaptiveCard{Schema: "http://adaptivecards.io/schemas/adaptive-card.json",
		Type: "AdaptiveCard", Version: "1.4", Body: blocks}
	if link := ch.Config["dashboard_url"]; link != "" {
		card.Actions = []teamsAction{{Type: "Action.OpenUrl", Title: "Open in pg_sage", URL: link}}
	}
	return teamsMessage{Type: "message", Attachments: []teamsAttachment{{
		ContentType: "application/vnd.microsoft.card.adaptive", Content: card}}}
}

func teamsColor(severity string) string {
	switch severity {
	case "critical":
		return "Attention"
	case "warning":
		return "Warning"
	default:
		return "Default"
	}
}
