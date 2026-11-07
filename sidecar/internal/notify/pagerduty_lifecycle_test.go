package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// alerting-01: the PagerDuty lifecycle of one incident or one critical
// finding must be one PagerDuty incident -- trigger, re-trigger, resolve
// on a single dedup_key.

// pdCapture is a fake PagerDuty Events API that records every request.
type pdCapture struct {
	mu     sync.Mutex
	events []pdEvent
	srv    *httptest.Server
}

func newPDCapture(t *testing.T) *pdCapture {
	t.Helper()
	c := &pdCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var ev pdEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *pdCapture) sender() *PagerDutySender {
	return &PagerDutySender{client: c.srv.Client(), apiURL: c.srv.URL}
}

func (c *pdCapture) captured() []pdEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]pdEvent(nil), c.events...)
}

func sendAll(t *testing.T, s *PagerDutySender, ch Channel, evts ...Event) {
	t.Helper()
	for _, evt := range evts {
		if err := s.Send(context.Background(), ch, evt); err != nil {
			t.Fatalf("send %s: %v", evt.Type, err)
		}
	}
}

func pdChannel(extra map[string]string) Channel {
	cfg := map[string]string{"routing_key": "rk"}
	for k, v := range extra {
		cfg[k] = v
	}
	return Channel{Name: "pd", Type: "pagerduty", Config: cfg}
}

func TestPagerDutyIncidentLifecycleSharesOneKey(t *testing.T) {
	pd := newPDCapture(t)
	info := sampleIncident()
	detected := IncidentDetectedEvent(info)
	info.Severity = "critical"
	escalated := IncidentEscalatedEvent(info)
	info.ResolvedBy = "pg_sage:auto"
	resolved := IncidentResolvedEvent(info)
	sendAll(t, pd.sender(), pdChannel(nil), detected, escalated, resolved)

	got := pd.captured()
	if len(got) != 3 {
		t.Fatalf("captured %d PagerDuty events, want 3", len(got))
	}
	wantKey := "incident:orders:" + info.ID
	wantActions := []string{"trigger", "trigger", "resolve"}
	for i, ev := range got {
		if ev.DedupKey != wantKey {
			t.Errorf("event %d dedup_key = %q, want %q", i, ev.DedupKey, wantKey)
		}
		if ev.EventAction != wantActions[i] {
			t.Errorf("event %d action = %q, want %q",
				i, ev.EventAction, wantActions[i])
		}
	}
	// Escalation re-triggers the same incident at the new severity.
	if got[1].Payload.Severity != "critical" {
		t.Errorf("escalation severity = %q, want critical",
			got[1].Payload.Severity)
	}
}

func TestPagerDutyIncidentKeyKeepsChannelPrefix(t *testing.T) {
	pd := newPDCapture(t)
	ch := pdChannel(map[string]string{"dedup_key_prefix": "env-prod"})
	info := sampleIncident()
	sendAll(t, pd.sender(), ch,
		IncidentDetectedEvent(info), IncidentResolvedEvent(info))
	got := pd.captured()
	want := "env-prod:incident:orders:" + info.ID
	if len(got) != 2 || got[0].DedupKey != want || got[1].DedupKey != want {
		t.Fatalf("captured = %+v, want two events keyed %q", got, want)
	}
	if got[1].EventAction != "resolve" {
		t.Fatalf("second action = %q, want resolve", got[1].EventAction)
	}
}

// A resolve that PagerDuty never saw a trigger for (rule added later, or
// the detection was filtered) is one plain resolve request: no trigger is
// synthesized, so no page is opened.
func TestPagerDutyResolveWithoutPriorTriggerIsHarmless(t *testing.T) {
	pd := newPDCapture(t)
	sendAll(t, pd.sender(), pdChannel(nil),
		IncidentResolvedEvent(sampleIncident()))
	got := pd.captured()
	if len(got) != 1 {
		t.Fatalf("captured %d events, want 1", len(got))
	}
	if got[0].EventAction != "resolve" {
		t.Fatalf("action = %q, want resolve", got[0].EventAction)
	}
	if got[0].DedupKey != "incident:orders:"+sampleIncident().ID {
		t.Fatalf("dedup_key = %q", got[0].DedupKey)
	}
}

// A recurrence is a new incident row with a new id: it must open a new
// PagerDuty incident rather than re-trigger the resolved one.
func TestPagerDutyRecurrenceGetsNewKey(t *testing.T) {
	pd := newPDCapture(t)
	first := sampleIncident()
	again := sampleIncident()
	again.ID = "8d3c0e1a-0000-4000-8000-000000000002"
	again.PreviousIncidentID = first.ID
	sendAll(t, pd.sender(), pdChannel(nil),
		IncidentDetectedEvent(first), IncidentResolvedEvent(first),
		IncidentDetectedEvent(again))
	got := pd.captured()
	if len(got) != 3 {
		t.Fatalf("captured %d events, want 3", len(got))
	}
	if got[2].DedupKey == got[0].DedupKey {
		t.Fatalf("recurrence reused key %q", got[2].DedupKey)
	}
	if got[2].DedupKey != "incident:orders:"+again.ID ||
		got[2].EventAction != "trigger" {
		t.Fatalf("recurrence event = %+v", got[2])
	}
}

func TestPagerDutyCriticalFindingTriggerAndResolveShareKey(t *testing.T) {
	pd := newPDCapture(t)
	trigger := FindingCriticalEvent("XID wraparound", "age 1.9B", "billing",
		"xid_wraparound", "current_database")
	resolved := ResolvedEvent(FindingCriticalEvent("XID wraparound (cleared)",
		"", "billing", "xid_wraparound", "current_database"))
	sendAll(t, pd.sender(), pdChannel(nil), trigger, resolved)
	got := pd.captured()
	want := "finding_critical:billing:xid_wraparound:current_database"
	if len(got) != 2 {
		t.Fatalf("captured %d events, want 2", len(got))
	}
	if got[0].DedupKey != want || got[1].DedupKey != want {
		t.Fatalf("keys = %q, %q, want both %q",
			got[0].DedupKey, got[1].DedupKey, want)
	}
	if got[0].EventAction != "trigger" || got[1].EventAction != "resolve" {
		t.Fatalf("actions = %q, %q, want trigger, resolve",
			got[0].EventAction, got[1].EventAction)
	}
	if got[1].Payload.Class != "finding_critical" {
		t.Fatalf("resolve class = %q, want finding_critical (same rule)",
			got[1].Payload.Class)
	}
	if !strings.HasPrefix(got[1].Payload.Summary, "Resolved: ") {
		t.Fatalf("resolve summary = %q", got[1].Payload.Summary)
	}
}

// Must not change: the explicit keys of approval, fact and SRE proposal
// cards are what their own lifecycles resolve against.
func TestPagerDutyExplicitCardKeysUnchanged(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"approval:orders:42", "approval:orders:42"},
		{"fact:orders:7", "fact:orders:7"},
		{"sre_proposal:p-1", "sre_proposal:p-1"},
	} {
		evt := Event{Type: "approval_needed", Severity: "warning",
			Subject: "x", DedupKey: tc.key,
			Data: map[string]any{"database": "orders"}}
		if got := pdDedupKey(t, pdChannel(nil), evt); got != tc.want {
			t.Errorf("dedup_key = %q, want %q", got, tc.want)
		}
	}
}
