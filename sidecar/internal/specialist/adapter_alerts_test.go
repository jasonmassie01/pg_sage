package specialist

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Review additions (mcp-02): the alert window a vendor timestamp gives,
// catalog-backed incident.io routing fields, Azure conditions other than
// Fired, and evidence a chain cites twice.

// No concurrent access tests here: withAlertWindow and agentSummary are
// pure functions of their arguments; the adapters' concurrent deliveries
// are covered by the RedeliveriesOf/TenFirings tests.

func TestWithAlertWindow_KeepsAValidStartAndDropsARefusedOne(t *testing.T) {
	now := created.Add(time.Hour)
	for _, tc := range []struct {
		name  string
		start time.Time
		keep  bool
	}{
		{"zero start", time.Time{}, false},
		{"recent start", now.Add(-10 * time.Minute), true},
		{"just inside 7 days", now.Add(-windowMaxAge + time.Minute), true},
		{"older than 7 days", now.Add(-windowMaxAge - time.Minute), false},
		{"inside the future skew", now.Add(windowFutureSkew - time.Second), true},
		{"beyond the future skew", now.Add(windowFutureSkew + time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := withAlertWindow(OpenRequest{Family: "lock_blocking"}, tc.start, now)
			if req.Family != "lock_blocking" {
				t.Fatalf("the request was replaced: %+v", req)
			}
			switch {
			case tc.keep && (req.Window == nil || !req.Window.Start.Equal(tc.start)):
				t.Fatalf("want window starting %s, got %+v", tc.start, req.Window)
			case !tc.keep && req.Window != nil:
				t.Fatalf("want no window, got %+v", req.Window)
			}
		})
	}
}

func TestIncidentIO_OldIncidentOpensWithoutAWindow(t *testing.T) {
	h, hs := ioHandler(t)
	body := []byte(strings.Replace(string(ioEvent(ioCreated, "01HOLD", "Database",
		"orders")), "2026-10-04T11:52:00Z", "2026-09-20T11:52:00Z", 1))
	w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
	if w.Code != 201 {
		t.Fatalf("an old incident reopened must still open: %d %s", w.Code, w.Body.String())
	}
	if rec := hs.store.all()[0]; rec.Window != nil {
		t.Fatalf("a window the contract refuses is dropped: %+v", rec.Window)
	}
}

func TestIncidentIO_CatalogFieldValueRoutes(t *testing.T) {
	h, hs := ioHandler(t)
	body := []byte(`{"event_type":"` + ioCreated + `","` + ioCreated + `":{"id":"01HCAT",
		"name":"orders slow","created_at":"2026-10-04T11:52:00Z","custom_field_entries":[
		{"custom_field":{"name":"Database"},"values":[{"value_catalog_entry":
		{"id":"01CE","name":"Orders"}}]}]}}`)
	w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
	if w.Code != 201 || len(hs.orders.started) != 1 {
		t.Fatalf("a catalog entry's name routes like an option: %d %s", w.Code,
			w.Body.String())
	}
}

func TestIncidentIO_EmptyCatalogEntryIsUnknownDatabase(t *testing.T) {
	h, hs := ioHandler(t)
	body := []byte(`{"event_type":"` + ioCreated + `","` + ioCreated + `":{"id":"01HCE",
		"name":"x","custom_field_entries":[{"custom_field":{"name":"Database"},
		"values":[{"value_catalog_entry":{"id":"01CE","name":"  "}}]}]}}`)
	w := postAdapter(h, ioPath, "propose-token", body, ioHeaders(hs, body))
	var ig WebhookIgnored
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &ig) != nil ||
		ig.Code != CodeUnknownDatabase || len(hs.orders.started) != 0 {
		t.Fatalf("a blank catalog entry routes nowhere: %d %s", w.Code, w.Body.String())
	}
}

func TestAzureMonitor_ConditionsOtherThanFiredAreIgnored(t *testing.T) {
	for _, condition := range []string{"", "fired", "Acknowledged"} {
		h, hs := azHandler(t)
		w := postAdapter(h, azPath, "propose-token",
			azAlert(azAlertID(azAlertGUID), condition, azResource), azKey)
		var ig WebhookIgnored
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &ig) != nil || !ig.Ignored ||
			ig.Code != "" {
			t.Fatalf("condition %q: %d %s", condition, w.Code, w.Body.String())
		}
		if len(hs.orders.started) != 0 {
			t.Fatalf("condition %q opened an investigation", condition)
		}
	}
}

func TestAgentSummary_EvidenceCitedTwiceIsListedOnce(t *testing.T) {
	chain := []ChainLink{
		{Ordinal: 1, Evidence: []Citation{{EvidenceID: "e1",
			Numbers: map[string]float64{"pid": 7}}}},
		{Ordinal: 2, Evidence: []Citation{{EvidenceID: "e2"}, {EvidenceID: "e1",
			Numbers: map[string]float64{"pid": 8}}}},
	}
	got := chainEvidence(chain)
	if len(got) != 2 || got[0].EvidenceID != "e1" || got[0].Numbers["pid"] != 7 ||
		got[1].EvidenceID != "e2" || got[1].Numbers == nil || len(got[1].Numbers) != 0 {
		t.Fatalf("each citation once, root first, numbers never null: %+v", got)
	}
	chain[0].Evidence[0].Numbers["pid"] = 99
	if got[0].Numbers["pid"] != 7 {
		t.Fatal("the summary's numbers alias the result's map")
	}
}
