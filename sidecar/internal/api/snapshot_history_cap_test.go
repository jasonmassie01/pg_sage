package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// History of a per-object category (one document per collector cycle
// listing every table, index, sequence...) pulled 666 MB to 8 GB in one
// request on lifeos. History serves bounded categories only.
func TestSnapshotHistory_RejectsPerObjectCategories(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	h := snapshotHistoryHandler(phase2MgrWithPool(pool))
	for _, metric := range []string{"tables", "indexes", "queries", "sequences",
		"foreign_keys", "locks", "partitions", "config_data"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET",
			"/api/v1/snapshots/history?database=testdb&metric="+metric, nil))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "snapshots/latest") {
			t.Errorf("%s: code %d body %s, want 400 pointing at /snapshots/latest",
				metric, w.Code, w.Body.String())
		}
	}
}

func TestSnapshotHistory_BoundedCategoryReturnsPoints(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (category, data, collected_at)
		SELECT 'system', jsonb_build_object('total_backends', g), now() - g * interval '1 minute'
		  FROM generate_series(1, 3) g`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	snapshotHistoryHandler(phase2MgrWithPool(pool)).ServeHTTP(w, httptest.NewRequest("GET",
		"/api/v1/snapshots/history?database=testdb&metric=system&hours=1", nil))
	var resp struct {
		Points []struct {
			Data map[string]any `json:"data"`
		} `json:"points"`
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || w.Code != 200 {
		t.Fatalf("code %d decode %v", w.Code, err)
	}
	if len(resp.Points) != 3 || resp.Truncated {
		t.Fatalf("points = %d truncated = %v, want 3 complete", len(resp.Points), resp.Truncated)
	}
	// Oldest first.
	if resp.Points[0].Data["total_backends"] != 3.0 || resp.Points[2].Data["total_backends"] != 1.0 {
		t.Fatalf("points out of order: %+v", resp.Points)
	}
}

// A bounded category can still be large in aggregate (500 points); the
// response stops at the byte cap and says so.
func TestSnapshotHistory_ResponseIsByteCapped(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	const docBytes = 700 << 10
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (category, data, collected_at)
		SELECT 'io', jsonb_build_object('pad', repeat('x', $1), 'n', g),
		       now() - g * interval '1 minute'
		  FROM generate_series(1, 8) g`, docBytes); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	snapshotHistoryHandler(phase2MgrWithPool(pool)).ServeHTTP(w, httptest.NewRequest("GET",
		"/api/v1/snapshots/history?database=testdb&metric=io&hours=1", nil))
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	if w.Body.Len() > snapshotHistoryMaxBytes+(64<<10) {
		t.Fatalf("response %d bytes, cap %d", w.Body.Len(), snapshotHistoryMaxBytes)
	}
	var resp struct {
		Points    []json.RawMessage `json:"points"`
		Truncated bool              `json:"truncated"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || len(resp.Points) == 0 || len(resp.Points) >= 8 {
		t.Fatalf("points = %d truncated = %v, want a partial, flagged response",
			len(resp.Points), resp.Truncated)
	}
}

// pointData decodes a history point's document as a JSON object.
func pointData(p historyPoint) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(p.Data, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}
