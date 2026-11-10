package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// alerting-02: fleet alerts must name their database, and PagerDuty
// incidents of identical schemas in different databases must not merge.
//
// No concurrent-access test here: buildPayload is a pure function of its
// Alert and the Manager stamps an immutable name; the per-database
// throttle isolation is covered against real PostgreSQL in
// alerting_db_test.go.

type pdEvent struct {
	EventAction string `json:"event_action"`
	DedupKey    string `json:"dedup_key"`
	Payload     struct {
		Source    string `json:"source"`
		Component string `json:"component"`
	} `json:"payload"`
}

func decodePD(t *testing.T, raw []byte) pdEvent {
	t.Helper()
	var ev pdEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode pagerduty payload %s: %v", raw, err)
	}
	return ev
}

func managerFor(database string) *Manager {
	return New(nil, ManagerConfig{DatabaseName: database}, nil, noop)
}

func pdFor(t *testing.T, m *Manager, f AlertFinding, resolved bool) pdEvent {
	t.Helper()
	alert := m.newAlert("critical", f)
	alert.Resolved = resolved
	raw, err := NewPagerDuty("shared-rk", legacyTestClient, noop).buildPayload(alert)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	return decodePD(t, raw)
}

func TestManagerDatabaseNameAccessor(t *testing.T) {
	if got := managerFor("orders").DatabaseName(); got != "orders" {
		t.Fatalf("DatabaseName() = %q, want orders", got)
	}
	if got := managerFor("").DatabaseName(); got != "" {
		t.Fatalf("DatabaseName() = %q, want empty", got)
	}
}

func TestPagerDutyDedupKey_DatabasePrefixSeparatesFleet(t *testing.T) {
	f := sampleFinding(1, "critical")
	a := pdFor(t, managerFor("orders"), f, false)
	b := pdFor(t, managerFor("billing"), f, false)
	if a.DedupKey != "orders:test_category:obj_1" {
		t.Errorf("orders key = %q", a.DedupKey)
	}
	if b.DedupKey != "billing:test_category:obj_1" {
		t.Errorf("billing key = %q", b.DedupKey)
	}
	if a.DedupKey == b.DedupKey {
		t.Fatalf("two databases share PagerDuty key %q", a.DedupKey)
	}
	if a.Payload.Component != "orders" || a.Payload.Source != "pg_sage:orders" {
		t.Errorf("payload component/source = %q/%q",
			a.Payload.Component, a.Payload.Source)
	}
}

func TestSlackHeaderNamesDatabase(t *testing.T) {
	f := sampleFinding(1, "critical")
	for _, resolved := range []bool{false, true} {
		alert := managerFor("orders").newAlert("critical", f)
		alert.Resolved = resolved
		slack := NewSlack("https://hooks.slack.com/x", legacyTestClient, noop)
		raw, err := slack.buildPayload(alert)
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		var p struct {
			Blocks []struct {
				Text struct{ Text string } `json:"text"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode slack: %v", err)
		}
		if len(p.Blocks) == 0 || !strings.Contains(p.Blocks[0].Text.Text, "orders") {
			t.Fatalf("resolved=%v slack header lacks database: %s", resolved, raw)
		}
	}
}

func TestPagerDutyDedupKey_ResolveCarriesOwnDatabase(t *testing.T) {
	f := sampleFinding(7, "critical")
	trigA := pdFor(t, managerFor("orders"), f, false)
	trigB := pdFor(t, managerFor("billing"), f, false)
	resA := pdFor(t, managerFor("orders"), f, true)
	if resA.EventAction != "resolve" {
		t.Fatalf("resolve action = %q", resA.EventAction)
	}
	if resA.DedupKey != trigA.DedupKey {
		t.Fatalf("resolve key %q != trigger key %q", resA.DedupKey, trigA.DedupKey)
	}
	if resA.DedupKey == trigB.DedupKey {
		t.Fatalf("orders resolve would close billing's page %q", trigB.DedupKey)
	}
}

func TestPagerDutyDedupKey_EmptyDatabaseUnchanged(t *testing.T) {
	f := sampleFinding(3, "critical")
	for _, resolved := range []bool{false, true} {
		ev := pdFor(t, managerFor(""), f, resolved)
		want := FormatDedupKey(f.Category, f.ObjectIdentifier)
		if ev.DedupKey != want || want != "test_category:obj_3" {
			t.Fatalf("resolved=%v key = %q, want %q", resolved, ev.DedupKey, want)
		}
		if ev.Payload.Source != "pg_sage" || ev.Payload.Component != "" {
			t.Fatalf("source/component = %q/%q", ev.Payload.Source, ev.Payload.Component)
		}
	}
}

func TestPagerDutyDedupKey_DatabaseWithColonKeptVerbatim(t *testing.T) {
	ev := pdFor(t, managerFor("shard:01"), sampleFinding(2, "critical"), false)
	if ev.DedupKey != "shard:01:test_category:obj_2" {
		t.Fatalf("key = %q", ev.DedupKey)
	}
}

func TestPagerDutyDedupKey_LongIdentityStaysWithinLimit(t *testing.T) {
	f := sampleFinding(4, "critical")
	f.ObjectIdentifier = "public." + strings.Repeat("o", 300)
	dbA := strings.Repeat("d", 62) + "a"
	dbB := strings.Repeat("d", 62) + "b"
	trig := pdFor(t, managerFor(dbA), f, false)
	res := pdFor(t, managerFor(dbA), f, true)
	other := pdFor(t, managerFor(dbB), f, false)
	if len(trig.DedupKey) > 255 {
		t.Fatalf("key length %d exceeds the PagerDuty limit", len(trig.DedupKey))
	}
	if !strings.HasPrefix(trig.DedupKey, dbA+":test_category:") {
		t.Errorf("bounded key lost its readable head: %q", trig.DedupKey)
	}
	if res.DedupKey != trig.DedupKey {
		t.Errorf("bounded resolve key %q != trigger key %q", res.DedupKey, trig.DedupKey)
	}
	if other.DedupKey == trig.DedupKey {
		t.Errorf("bounded keys of two databases collide: %q", trig.DedupKey)
	}
}

func TestPagerDutyBuildPayload_NoFindingsStillRejected(t *testing.T) {
	alert := managerFor("orders").newAlert("critical")
	if _, err := NewPagerDuty("rk", legacyTestClient, noop).buildPayload(alert); err == nil {
		t.Fatal("alert without findings built a PagerDuty payload")
	}
}

// captureTransport records PagerDuty requests without any network call.
type captureTransport struct {
	mu     sync.Mutex
	events []pdEvent
	hosts  []string
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var ev pdEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.hosts = append(c.hosts, r.URL.Host)
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{},
		Body:    io.NopCloser(bytes.NewReader([]byte(`{"status":"success"}`))),
		Request: r}, nil
}

func (c *captureTransport) snapshot() []pdEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]pdEvent(nil), c.events...)
}

func capturingPagerDuty() (*PagerDutyChannel, *captureTransport) {
	tr := &captureTransport{}
	ch := NewPagerDuty("shared-rk", legacyTestClient, noop)
	ch.client = &http.Client{Transport: tr}
	return ch, tr
}

// TestPagerDutySend_CapturedPayloadNamesDatabase is CHECK-16: the payload
// actually sent carries the database-prefixed key and the component.
func TestPagerDutySend_CapturedPayloadNamesDatabase(t *testing.T) {
	ch, tr := capturingPagerDuty()
	alert := managerFor("orders").newAlert("critical", sampleFinding(9, "critical"))
	if err := ch.Send(context.Background(), alert); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := tr.snapshot()
	if len(got) != 1 {
		t.Fatalf("captured %d PagerDuty events, want 1", len(got))
	}
	if got[0].DedupKey != "orders:test_category:obj_9" ||
		got[0].Payload.Component != "orders" || got[0].EventAction != "trigger" {
		t.Fatalf("captured event = %+v", got[0])
	}
	if tr.hosts[0] != "events.pagerduty.com" {
		t.Fatalf("posted to host %q", tr.hosts[0])
	}
}
