package notify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ux-09a: notification classes Page, Ask and Digest. Class rules route by
// class; Ask is batched per database through the outbox; Digest-class
// events reach a channel only as the digest.

func TestClassOf_MapCoversEveryEventType(t *testing.T) {
	for event := range ValidEventTypes {
		if _, ok := eventClasses[event]; !ok {
			t.Errorf("event %q has no class", event)
		}
	}
	page := map[string]bool{"finding_critical": true, "incident_detected": true,
		"incident_escalated": true, "custodian_escalation": true, "canary_interrupted": true,
		EventInvestigationConcluded: true}
	for event, c := range eventClasses {
		if !c.Valid() {
			t.Errorf("event %q has invalid class %q", event, c)
		}
		if page[event] != (c == ClassPage) {
			t.Errorf("event %q is %s, want page=%v", event, c, page[event])
		}
	}
	if eventClasses["approval_needed"] != ClassAsk {
		t.Fatalf("approval_needed is %s, want ask", eventClasses["approval_needed"])
	}
}

func TestClassOf_CriticalAndRollbackFailureArePage(t *testing.T) {
	cases := []struct {
		evt  Event
		want Class
	}{
		{Event{Type: "finding_critical", Severity: "critical"}, ClassPage},
		{Event{Type: "approval_needed", Severity: "warning"}, ClassAsk},
		{Event{Type: "action_executed", Severity: "info"}, ClassDigest},
		{Event{Type: "action_failed", Severity: "warning"}, ClassDigest},
		{Event{Type: "action_failed", Severity: "warning",
			Data: map[string]any{DataRollbackFailed: true}}, ClassPage},
		{Event{Type: "action_failed", Severity: "warning",
			Data: map[string]any{DataRollbackFailed: "yes"}}, ClassDigest},
		// An unmapped critical event is Page (fail safe); otherwise Digest.
		{Event{Type: "brand_new_event", Severity: "critical"}, ClassPage},
		{Event{Type: "brand_new_event", Severity: "warning"}, ClassDigest},
		{Event{}, ClassDigest},
		// A critical event of an Ask or Digest type is still Page.
		{Event{Type: "finding_opened", Severity: "critical"}, ClassPage},
	}
	for _, c := range cases {
		if got := ClassOf(c.evt); got != c.want {
			t.Errorf("ClassOf(%s/%s %v) = %s, want %s", c.evt.Type, c.evt.Severity,
				c.evt.Data, got, c.want)
		}
	}
	if EventClass("finding_critical") != ClassPage || EventClass("fact_expired") != ClassDigest {
		t.Fatal("EventClass does not follow each event's own severity")
	}
}

func TestParseClass_RefusesUnknownClasses(t *testing.T) {
	for _, s := range []string{"page", "ask", "digest"} {
		if c, err := ParseClass(s); err != nil || string(c) != s {
			t.Fatalf("ParseClass(%q) = %q, %v", s, c, err)
		}
	}
	for _, s := range []string{"", "Page", "pager", "all"} {
		if _, err := ParseClass(s); err == nil || !strings.Contains(err.Error(), "page, ask") {
			t.Fatalf("ParseClass(%q) error %v, want the allowed classes", s, err)
		}
	}
	if ClassRuleEvent(ClassAsk) != "class:ask" || ValidEventTypes[ClassRuleEvent(ClassAsk)] {
		t.Fatal("a class rule's event must be class:<name> and never an event type")
	}
}

// classRig is an outbox rig with one webhook channel on class rules.
type classRig struct {
	*outboxRig
	seq *statusSeq
	ch  int
}

func newClassRig(t *testing.T, window time.Duration, statuses ...int) *classRig {
	t.Helper()
	if len(statuses) == 0 {
		statuses = []int{200}
	}
	rig := &classRig{outboxRig: newOutboxRig(t, "", ""), seq: &statusSeq{statuses: statuses}}
	srv := rig.seq.server(t)
	rig.ch = rig.store.AddChannel(Channel{Name: "hook", Type: "webhook",
		Config: map[string]string{"webhook_url": srv.URL}, Enabled: true})
	for _, c := range Classes() {
		rig.store.AddRule(Rule{ChannelID: rig.ch, Event: ClassRuleEvent(c),
			DeliveryClass: string(c), MinSeverity: "info", Enabled: true})
	}
	rig.d.SetAskBatch(window)
	return rig
}

func criticalEvent() Event {
	return FindingCriticalEvent("Critical bloat", "detail", "orders", "bloat", "public.t")
}

func askEvent(db string, n int) Event {
	evt := ApprovalNeededEvent(fmt.Sprintf("Index %d", n), "SELECT 1", db, "moderate")
	evt.DedupKey = fmt.Sprintf("approval:%s:%d", db, n)
	return evt
}

func (r *classRig) submit(t *testing.T, evt Event) DispatchResult {
	t.Helper()
	res, err := r.d.Submit(context.Background(), evt)
	if err != nil {
		t.Fatalf("Submit(%s): %v", evt.Type, err)
	}
	return res
}

func (r *classRig) subjects() []string {
	r.seq.mu.Lock()
	defer r.seq.mu.Unlock()
	var out []string
	for _, b := range r.seq.bodies {
		s, _ := b["subject"].(string)
		out = append(out, s)
	}
	return out
}

func (r *classRig) lastBody(t *testing.T) string {
	t.Helper()
	r.seq.mu.Lock()
	defer r.seq.mu.Unlock()
	if len(r.seq.bodies) == 0 {
		t.Fatal("nothing was delivered")
	}
	body, _ := r.seq.bodies[len(r.seq.bodies)-1]["body"].(string)
	return body
}

func TestClassRules_AskIsBatchedAndPageGoesAtOnce(t *testing.T) {
	rig := newClassRig(t, 15*time.Minute)
	rig.submit(t, askEvent("orders", 1))
	rig.clock.Advance(time.Minute)
	rig.submit(t, askEvent("orders", 2))
	rig.submit(t, criticalEvent())
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "Critical") {
		t.Fatalf("before the window ends sent %q, want only the page", got)
	}
	rig.clock.Advance(13 * time.Minute)
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 {
		t.Fatalf("the Ask batch went out before its window ended: %q", got)
	}
	rig.clock.Advance(time.Minute)
	rig.drain(t)
	got := rig.subjects()
	if len(got) != 2 || !strings.Contains(got[1], "2 decisions") ||
		!strings.Contains(got[1], "orders") {
		t.Fatalf("after the window sent %q, want one batched Ask message", got)
	}
	body := rig.lastBody(t)
	for _, want := range []string{"Index 1", "Index 2", "#/inbox"} {
		if !strings.Contains(body, want) {
			t.Fatalf("batch body %q lacks %q", body, want)
		}
	}
}

func TestClassRules_AskBatchesArePerDatabase(t *testing.T) {
	rig := newClassRig(t, 15*time.Minute)
	rig.submit(t, askEvent("orders", 1))
	rig.submit(t, askEvent("billing", 2))
	rig.clock.Advance(15 * time.Minute)
	rig.drain(t)
	if got := rig.subjects(); len(got) != 2 {
		t.Fatalf("two databases' asks sent %q, want one message each", got)
	}
}

func TestClassRules_AskBatchBoundaries(t *testing.T) {
	rig := newClassRig(t, 15*time.Minute)
	for i := 1; i <= 11; i++ {
		rig.submit(t, askEvent("orders", i))
	}
	rig.clock.Advance(15 * time.Minute)
	rig.drain(t)
	body := rig.lastBody(t)
	if !strings.Contains(body, "Index 10") || strings.Contains(body, "Index 11") ||
		!strings.Contains(body, "1 more in pg_sage") {
		t.Fatalf("11 asks rendered %q, want 10 listed and 1 more", body)
	}
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "11 decisions") {
		t.Fatalf("11 asks sent %q, want one message counting 11", got)
	}
	ten := newClassRig(t, 15*time.Minute)
	for i := 1; i <= 10; i++ {
		ten.submit(t, askEvent("orders", i))
	}
	ten.clock.Advance(15 * time.Minute)
	ten.drain(t)
	if body := ten.lastBody(t); !strings.Contains(body, "Index 10") ||
		strings.Contains(body, "more in pg_sage") {
		t.Fatalf("10 asks rendered %q, want all 10 and no 'more'", body)
	}
}

func TestClassRules_ZeroWindowSendsEachAskAlone(t *testing.T) {
	rig := newClassRig(t, 0)
	rig.submit(t, askEvent("orders", 1))
	rig.submit(t, askEvent("orders", 2))
	rig.drain(t)
	got := rig.subjects()
	if len(got) != 2 || !strings.Contains(got[0], "Index 1") ||
		!strings.Contains(got[1], "Index 2") {
		t.Fatalf("window 0 sent %q, want each ask alone", got)
	}
}

// A single Ask in its window goes out as itself, so its card (and its
// buttons) survive the batching.
func TestClassRules_ALoneAskKeepsItsOwnEvent(t *testing.T) {
	rig := newClassRig(t, 15*time.Minute)
	rig.submit(t, askEvent("orders", 1))
	rig.clock.Advance(15 * time.Minute)
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "Index 1") {
		t.Fatalf("a lone ask sent %q, want the ask itself", got)
	}
	// The next ask, after the window, opens a new batch.
	rig.submit(t, askEvent("orders", 2))
	rig.clock.Advance(15 * time.Minute)
	rig.drain(t)
	if got := rig.subjects(); len(got) != 2 || !strings.Contains(got[1], "Index 2") {
		t.Fatalf("the second window sent %q", got)
	}
}

func TestClassRules_DigestClassSendsOnlyTheDigest(t *testing.T) {
	rig := newClassRig(t, 15*time.Minute)
	rig.submit(t, ActionExecutedEvent("Analyze", "ANALYZE t", "orders"))
	rig.submit(t, Event{Type: EventDigestReady, Severity: "info", Subject: "Daily digest",
		Data: map[string]any{"database": "orders"}})
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || got[0] != "Daily digest" {
		t.Fatalf("digest class sent %q, want only the digest", got)
	}
}

// A channel with both a legacy event rule and a class rule gets one copy.
func TestClassRules_LegacyAndClassRuleOnOneChannelSendOnce(t *testing.T) {
	rig := newClassRig(t, 0)
	rig.store.AddRule(Rule{ChannelID: rig.ch, Event: "finding_critical", MinSeverity: "info",
		Enabled: true})
	rig.submit(t, criticalEvent())
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 {
		t.Fatalf("sent %q, want one copy", got)
	}
}

func TestClassRules_NoChannelSendsNothing(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	rig.d.SetAskBatch(15 * time.Minute)
	res, err := rig.d.Submit(context.Background(), askEvent("orders", 1))
	if err != nil || res != (DispatchResult{}) || len(rig.store.Outbox()) != 0 {
		t.Fatalf("no channel: %+v, %v, outbox %d", res, err, len(rig.store.Outbox()))
	}
	if ClassOf(askEvent("orders", 1)) != ClassAsk {
		t.Fatal("the class is still computed without a channel")
	}
}

// A Page whose first send fails is retried through the outbox.
func TestClassRules_PageRetriesThroughTheOutbox(t *testing.T) {
	rig := newClassRig(t, 15*time.Minute, 503, 200)
	rig.submit(t, criticalEvent())
	if res := rig.drain(t); res.Retried != 1 || res.Sent != 0 {
		t.Fatalf("first drain %+v, want one retry", res)
	}
	rig.clock.Advance(10 * time.Minute)
	if res := rig.drain(t); res.Sent != 1 {
		t.Fatalf("second drain %+v, want the page sent", res)
	}
}

// No concurrent access test here: the batch is decided inside the store's
// Schedule (a mutex in memory, an advisory lock in PostgreSQL); the
// concurrent case runs on PostgreSQL in class_db_test.go.
