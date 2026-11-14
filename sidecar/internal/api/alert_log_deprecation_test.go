package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// alerting-06c: one notification log. The legacy alert log stays readable
// but says it is deprecated and links its successor; the notification log
// read opens to the roles /alerts had (admins and operators) while channel
// and rule routes stay admin-only; ?database= filters it like other list
// endpoints, and a delivery of no database reads "unknown", never "all".
//
// No concurrent access tests: both handlers are stateless reads.
// No state transition tests: the log is append-only.

func logFleet(names ...string) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, name := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: false}})
	}
	return mgr
}

func assertDeprecated(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := w.Header().Get("Deprecation"); got != "true" {
		t.Errorf("Deprecation = %q, want true", got)
	}
	link := w.Header().Get("Link")
	if !strings.Contains(link, "</api/v1/notifications/log>") ||
		!strings.Contains(link, `rel="successor-version"`) {
		t.Errorf("Link = %q, want the notification log as successor-version", link)
	}
}

func TestAlertLog_IsDeprecatedAndLinksTheNotificationLog(t *testing.T) {
	w := httptest.NewRecorder()
	alertLogHandler(logFleet())(w, httptest.NewRequest("GET", "/api/v1/alert-log", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (history stays readable)", w.Code)
	}
	assertDeprecated(t, w)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["alerts"]; !ok {
		t.Fatalf("body = %v, want the alerts history", body)
	}
}

// Invalid input on the deprecated route still carries the headers.
func TestAlertLog_RefusalIsDeprecatedToo(t *testing.T) {
	w := httptest.NewRecorder()
	alertLogHandler(logFleet("db1"))(w,
		httptest.NewRequest("GET", "/api/v1/alert-log?database=bad%3Bname", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	assertDeprecated(t, w)
}

// Both gates: the log read is admin and operator, channels and rules
// (reads and writes) stay admin.
func TestNotificationRoutes_LogOpensToReviewersOnly(t *testing.T) {
	mux := http.NewServeMux()
	registerNotificationRoutes(mux, unreachablePool(t), notificationRouteDeps{})
	h := injectCharRole(mux)
	want := map[routeProbe]string{
		{"GET", "/api/v1/notifications/log"}:        "gate:operator",
		{"GET", "/api/v1/notifications/channels"}:   "gate:admin",
		{"GET", "/api/v1/notifications/rules"}:      "gate:admin",
		{"POST", "/api/v1/notifications/channels"}:  "gate:admin",
		{"PUT", "/api/v1/notifications/rules/7"}:    "gate:admin",
		{"DELETE", "/api/v1/notifications/rules/7"}: "gate:admin",
	}
	for probe, gate := range want {
		if got := classifyRoute(h, probe); got != gate {
			t.Errorf("%s %s = %s, want %s", probe.method, probe.path, got, gate)
		}
	}
}

func getLog(t *testing.T, ns *store.NotificationStore, mgr *fleet.DatabaseManager,
	query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	listNotificationLogHandler(ns, mgr)(w,
		httptest.NewRequest("GET", "/api/v1/notifications/log"+query, nil))
	return w
}

// Invalid input: a malformed name is 400; a well-formed name pg_sage does
// not monitor is 404 "database not found", as on /api/v1/alert-log.
func TestNotificationLog_RejectsBadDatabaseParam(t *testing.T) {
	ns := store.NewNotificationStore(nil, nil)
	mgr := logFleet("db1", "db2")
	if w := getLog(t, ns, mgr, "?database=bad%3Bname"); w.Code != http.StatusBadRequest {
		t.Errorf("malformed: status %d, want 400", w.Code)
	}
	long := strings.Repeat("x", 64)
	if w := getLog(t, ns, mgr, "?database="+long); w.Code != http.StatusBadRequest {
		t.Errorf("64-char name: status %d, want 400", w.Code)
	}
	w := getLog(t, ns, mgr, "?database=ghost")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "database not found") {
		t.Errorf("unknown: status %d body %s, want 404 database not found", w.Code, w.Body)
	}
}

func seedAPILog(t *testing.T) (*store.NotificationStore, string, string) {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	n := time.Now().UnixNano()
	db, subject := fmt.Sprintf("a06c_api_%d", n), fmt.Sprintf("a06c_api_log_%d", n)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.notification_log
		(event, subject, status, database, dedup_key) VALUES
		('finding_opened', $1, 'sent', NULL, NULL),
		('finding_opened', $1, 'sent', $2, 'finding:5')`, subject, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_log WHERE subject = $1`, subject)
	})
	return store.NewNotificationStore(pool, nil), db, subject
}

func decodeLog(t *testing.T, w *httptest.ResponseRecorder, subject string) []map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", w.Code, w.Body)
	}
	var reply struct {
		Log []map[string]any `json:"log"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var out []map[string]any
	for _, e := range reply.Log {
		if e["subject"] == subject {
			out = append(out, e)
		}
	}
	return out
}

// Nil/empty and boundary on real PostgreSQL: without a filter the NULL
// row reads "unknown" (never "all"); with ?database= only that database's
// row is listed; ?database=all is no filter.
func TestNotificationLog_ShowsUnknownAndFiltersByDatabase(t *testing.T) {
	ns, db, subject := seedAPILog(t)
	mgr := logFleet(db)
	for _, q := range []string{"?limit=500", "?database=all&limit=500"} {
		rows := decodeLog(t, getLog(t, ns, mgr, q), subject)
		if len(rows) != 2 {
			t.Fatalf("%q: %d rows, want 2", q, len(rows))
		}
		dbs := map[any]bool{rows[0]["database"]: true, rows[1]["database"]: true}
		if !dbs["unknown"] || !dbs[db] || dbs["all"] {
			t.Fatalf("%q: databases %v, want unknown and %s, never all", q, dbs, db)
		}
	}
	rows := decodeLog(t, getLog(t, ns, mgr, "?database="+db), subject)
	if len(rows) != 1 || rows[0]["database"] != db || rows[0]["dedup_key"] != "finding:5" {
		t.Fatalf("filtered rows = %v, want the one %s row with its dedup key", rows, db)
	}
}
