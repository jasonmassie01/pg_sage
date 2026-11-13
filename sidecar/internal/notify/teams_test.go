package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// market-14: Microsoft Teams through a Workflows incoming webhook that
// posts an Adaptive Card. The webhook URL is a channel secret and a
// TargetPolicy target.
// No concurrent access tests: the sender is stateless apart from its
// http.Client, which is safe for concurrent use.

func teamsChannel(url string) Channel {
	return Channel{ID: 7, Name: "dba-teams", Type: "teams",
		Config: map[string]string{"webhook_url": url}}
}

const teamsGoldenOpen = `{
 "type": "message",
 "attachments": [{
  "contentType": "application/vnd.microsoft.card.adaptive",
  "content": {
   "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
   "type": "AdaptiveCard",
   "version": "1.4",
   "body": [
    {"type": "TextBlock", "text": "Replication lag on db1", "weight": "Bolder",
     "size": "Medium", "wrap": true, "color": "Attention"},
    {"type": "FactSet", "facts": [
     {"title": "Severity", "value": "critical"},
     {"title": "Event", "value": "finding_opened"},
     {"title": "Database", "value": "db1"},
     {"title": "Dedup key", "value": "finding:db1:replication:lag"}]},
    {"type": "TextBlock", "text": "lag is 12 minutes", "wrap": true}
   ]
  }
 }]
}`

func TestTeamsSend_PostsGoldenAdaptiveCard(t *testing.T) {
	fake, srv := newDestFake(t, http.StatusAccepted)
	s := NewTeamsSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	if s.Type() != "teams" {
		t.Fatalf("Type = %q, want teams", s.Type())
	}
	err := s.Send(context.Background(), teamsChannel(srv.URL+"/wf"), destEvent(false))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/wf" {
		t.Fatalf("requests = %+v, want one POST /wf", reqs)
	}
	if ct := reqs[0].Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	assertJSONEqual(t, reqs[0].Body, teamsGoldenOpen)
}

func TestTeamsSend_ResolveIsAGreenResolvedCardWithLink(t *testing.T) {
	fake, srv := newDestFake(t, http.StatusOK)
	s := NewTeamsSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := teamsChannel(srv.URL)
	ch.Config["dashboard_url"] = "https://sage.example.com/cases"
	if err := s.Send(context.Background(), ch, destEvent(true)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	card := teamsCard(t, fake.requests()[0].Raw)
	if card.Body[0].Text != "Resolved: Replication lag on db1" || card.Body[0].Color != "Good" {
		t.Fatalf("title block = %+v, want a green Resolved title", card.Body[0])
	}
	if len(card.Actions) != 1 || card.Actions[0].URL != "https://sage.example.com/cases" ||
		card.Actions[0].Type != "Action.OpenUrl" {
		t.Fatalf("actions = %+v, want one OpenUrl to the dashboard", card.Actions)
	}
}

// teamsCard decodes the card a request carried.
func teamsCard(t *testing.T, raw []byte) teamsAdaptiveCard {
	t.Helper()
	var msg teamsMessage
	if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Attachments) != 1 {
		t.Fatalf("teams message %s: %v", raw, err)
	}
	return msg.Attachments[0].Content
}

func TestTeamsSend_OversizedBodyIsTruncatedWithSeePgSage(t *testing.T) {
	fake, srv := newDestFake(t, http.StatusAccepted)
	s := NewTeamsSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := teamsChannel(srv.URL)
	ch.Config["dashboard_url"] = "https://sage.example.com/cases"
	evt := destEvent(false)
	// Quotes and angle brackets grow under JSON escaping, so the cut must
	// measure the encoded payload, not the text.
	evt.Body = strings.Repeat(`é"<>`, teamsMaxPayloadBytes)
	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw := fake.requests()[0].Raw
	if len(raw) > teamsMaxPayloadBytes {
		t.Fatalf("payload is %d bytes, want at most %d", len(raw), teamsMaxPayloadBytes)
	}
	text := teamsCard(t, raw).Body[2].Text
	if !strings.HasSuffix(text, teamsTruncatedNote) || !utf8.ValidString(text) {
		t.Fatalf("body tail = %q, want the see-pg_sage note on valid UTF-8",
			text[max(0, len(text)-80):])
	}
	if len(raw) < teamsMaxPayloadBytes-512 {
		t.Fatalf("payload is %d bytes; the cut threw away far more than needed", len(raw))
	}
}

func TestTeamsSend_PayloadAtTheLimitIsNotTruncated(t *testing.T) {
	evt := destEvent(false)
	// A one-byte body, so the base card already carries the body block.
	evt.Body = "a"
	base, err := buildTeamsPayload(teamsChannel("https://x"), evt)
	if err != nil {
		t.Fatal(err)
	}
	evt.Body = strings.Repeat("a", teamsMaxPayloadBytes-len(base)+1)
	exact, err := buildTeamsPayload(teamsChannel("https://x"), evt)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != teamsMaxPayloadBytes || strings.Contains(string(exact), teamsTruncatedNote) {
		t.Fatalf("payload at the limit: %d bytes, truncated=%v; want %d and whole",
			len(exact), strings.Contains(string(exact), teamsTruncatedNote), teamsMaxPayloadBytes)
	}
	evt.Body += "a"
	over, err := buildTeamsPayload(teamsChannel("https://x"), evt)
	if err != nil {
		t.Fatal(err)
	}
	if len(over) > teamsMaxPayloadBytes || !strings.Contains(string(over), teamsTruncatedNote) {
		t.Fatalf("one byte over: %d bytes, want truncated within the limit", len(over))
	}
}

func TestTeamsSend_429CarriesRetryAfterAndIsRetryable(t *testing.T) {
	fake, srv := newDestFake(t, http.StatusTooManyRequests)
	fake.set(http.StatusTooManyRequests, "120")
	s := NewTeamsSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	err := s.Send(context.Background(), teamsChannel(srv.URL), destEvent(false))
	var se *HTTPStatusError
	if !errors.As(err, &se) || se.Code != 429 || se.Service != "teams" {
		t.Fatalf("err = %v, want a teams 429 status error", err)
	}
	if se.RetryAfter != 120*time.Second || !Retryable(err) {
		t.Fatalf("RetryAfter = %v retryable=%v, want 2m and retryable", se.RetryAfter,
			Retryable(err))
	}
}

func TestTeamsSend_400IsPermanent(t *testing.T) {
	_, srv := newDestFake(t, http.StatusBadRequest)
	s := NewTeamsSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	err := s.Send(context.Background(), teamsChannel(srv.URL), destEvent(false))
	if err == nil || Retryable(err) {
		t.Fatalf("err = %v, want a non-retryable failure", err)
	}
}

func TestTeamsSend_PrivateURLRefusedUnlessAllowed(t *testing.T) {
	fake, srv := newDestFake(t, http.StatusAccepted)
	strict := NewTeamsSender()
	err := strict.Send(context.Background(), teamsChannel(srv.URL), destEvent(false))
	if !errors.Is(err, ErrTargetBlocked) || Retryable(err) {
		t.Fatalf("strict err = %v, want ErrTargetBlocked, not retryable", err)
	}
	if n := len(fake.requests()); n != 0 {
		t.Fatalf("strict policy reached the private target %d time(s)", n)
	}
	open := NewTeamsSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	if err := open.Send(context.Background(), teamsChannel(srv.URL), destEvent(false)); err != nil {
		t.Fatalf("allow_private_targets send: %v", err)
	}
}

func TestTeamsSend_MissingURLIsPermanent(t *testing.T) {
	s := NewTeamsSender()
	err := s.Send(context.Background(), teamsChannel(""), destEvent(false))
	if !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "webhook_url") {
		t.Fatalf("err = %v, want a permanent missing webhook_url error", err)
	}
}

func TestTeamsSend_NilDataAndEmptyBodyStillPost(t *testing.T) {
	fake, srv := newDestFake(t, http.StatusAccepted)
	s := NewTeamsSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	evt := Event{Type: "test", Severity: "info", Subject: "hello"}
	if err := s.Send(context.Background(), teamsChannel(srv.URL), evt); err != nil {
		t.Fatalf("Send: %v", err)
	}
	card := teamsCard(t, fake.requests()[0].Raw)
	if len(card.Body) != 2 || card.Body[0].Text != "hello" || card.Body[0].Color != "Default" {
		t.Fatalf("card body = %+v, want title and facts only, default colour", card.Body)
	}
	if len(card.Body[1].Facts) != 3 {
		t.Fatalf("facts = %+v, want severity, event and dedup key (no database)",
			card.Body[1].Facts)
	}
}
