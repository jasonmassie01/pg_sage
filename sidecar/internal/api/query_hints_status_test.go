package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tuning-01 added the dormant hint status. The /query-hints status filter
// accepts every lifecycle status and still rejects anything else.

func getQueryHints(t *testing.T, h http.Handler, status string) (int, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/query-hints?database=testdb&status="+status, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	var resp struct {
		Hints []map[string]any `json:"hints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode status=%s: %v body=%s", status, err, w.Body.String())
	}
	return w.Code, resp.Hints
}

func TestQueryHintsHandler_FiltersEveryLifecycleStatus(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	statuses := []string{"proposed", "active", "dormant", "retired", "broken", "rolled_back"}
	for i, s := range statuses {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
			(queryid, hint_text, symptom, status) VALUES ($1, 'Use idx', 'seq_scan', $2)`,
			int64(500+i), s); err != nil {
			t.Fatalf("insert %s hint: %v", s, err)
		}
	}
	h := queryHintsHandler(phase2MgrWithPool(pool))
	for _, s := range statuses {
		code, hints := getQueryHints(t, h, s)
		if code != http.StatusOK {
			t.Fatalf("status=%s: HTTP %d, want 200", s, code)
		}
		if len(hints) != 1 || hints[0]["status"] != s {
			t.Fatalf("status=%s: hints = %+v, want exactly the %s hint", s, hints, s)
		}
	}
}

func TestQueryHintsHandler_RejectsUnknownStatus(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	h := queryHintsHandler(phase2MgrWithPool(pool))
	for _, s := range []string{"sleeping", "ACTIVE", "active'--"} {
		if code, _ := getQueryHints(t, h, s); code != http.StatusBadRequest {
			t.Fatalf("status=%q: HTTP %d, want 400", s, code)
		}
	}
}

// tuning-06 added the rejected and expired statuses; the filter accepts
// them.
func TestQueryHintsHandler_FiltersRejectedAndExpired(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	for i, s := range []string{"rejected", "expired"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
			(queryid, hint_text, symptom, status) VALUES ($1, 'Use idx', 'seq_scan', $2)`,
			int64(600+i), s); err != nil {
			t.Fatalf("insert %s hint: %v", s, err)
		}
	}
	h := queryHintsHandler(phase2MgrWithPool(pool))
	for _, s := range []string{"rejected", "expired"} {
		code, hints := getQueryHints(t, h, s)
		if code != http.StatusOK || len(hints) != 1 || hints[0]["status"] != s {
			t.Fatalf("status=%s: HTTP %d hints %+v, want exactly the %s hint", s, code,
				hints, s)
		}
	}
}
