package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/specialist"
)

// ux-05e: a request row carries queue_id when its remediation queued an
// approval, so the dashboard links the request to the approval it waits on.
// No concurrency test: the handler maps one read and keeps no state.

func auditItems(t *testing.T, recs []specialist.Record) []map[string]any {
	t.Helper()
	rt := &RuntimeDeps{SpecialistAudit: &auditStub{records: recs}}
	h := specialistRouter(t, rt, testOperatorUser(), false)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/specialist-requests", nil))
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("audit: %d %s", w.Code, w.Body.String())
	}
	return body.Items
}

func TestSpecialistAuditRoute_QueueID(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	items := auditItems(t, []specialist.Record{
		{ID: "q1", Kind: specialist.KindRemediation, Database: "orders",
			InvestigationID: "44444444-4444-4444-8444-444444444444",
			RemediationID:   "cancel_backend.55555555-5555-4555-8555-555555555555",
			Verdict:         "queued_for_approval", ApprovalQueueID: 4242,
			CreatedAt: at},
		{ID: "o1", Kind: specialist.KindOpen, Database: "orders", CreatedAt: at},
	})
	if len(items) != 2 {
		t.Fatalf("items %v", items)
	}
	if got, ok := items[0]["queue_id"].(float64); !ok || got != 4242 {
		t.Fatalf("queued row: queue_id %v (%T)", items[0]["queue_id"], items[0]["queue_id"])
	}
	if items[0]["remediation_id"] != "cancel_backend.55555555-5555-4555-8555-555555555555" ||
		items[0]["verdict"] != "queued_for_approval" {
		t.Fatalf("queued row %v", items[0])
	}
	// Nil: a row that queued nothing has no queue_id key at all.
	if _, has := items[1]["queue_id"]; has {
		t.Fatalf("open row carries queue_id: %v", items[1])
	}
}

func TestSpecialistAuditRoute_QueueIDRawNumber(t *testing.T) {
	rt := &RuntimeDeps{SpecialistAudit: &auditStub{records: []specialist.Record{
		{ID: "q2", Kind: specialist.KindRemediation, ApprovalQueueID: 42}}}}
	h := specialistRouter(t, rt, testOperatorUser(), false)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/specialist-requests", nil))
	if !strings.Contains(w.Body.String(), `"queue_id":42`) {
		t.Fatalf("queue_id is not a plain number: %s", w.Body.String())
	}
}

// Error propagation: the store outage path is unchanged by the new field.
func TestSpecialistAuditRoute_OutageKeepsItsAnswer(t *testing.T) {
	rt := &RuntimeDeps{SpecialistAudit: &auditStub{err: errors.New("connection refused"),
		records: []specialist.Record{{ID: "x", ApprovalQueueID: 7}}}}
	h := specialistRouter(t, rt, testOperatorUser(), false)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/specialist-requests", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("store outage: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "request audit unavailable") ||
		strings.Contains(body, "queue_id") || strings.Contains(body, "connection refused") {
		t.Fatalf("outage body %s", body)
	}
}
