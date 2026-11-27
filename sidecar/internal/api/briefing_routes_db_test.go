package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/config"
)

// llm-06b integration: the briefings routes over the fleet manager and a
// real sage.briefings table.

func clearAPIBriefings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clear := func() { _, _ = pool.Exec(context.Background(), "DELETE FROM sage.briefings") }
	clear()
	t.Cleanup(clear)
}

func getBriefings(t *testing.T, pool *pgxpool.Pool, path string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	registerBriefingRoutes(mux, phase2MgrWithPool(pool))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(httptest.NewRequest(http.MethodGet, path, nil), viewerUser()))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// Happy path: the latest briefing a worker stored comes back with its
// content_json exactly as stored.
func TestBriefingsLatestFromDatabaseRoundTripsContentJSON(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	clearAPIBriefings(t, pool)
	code, body := getBriefings(t, pool, "/api/v1/briefings/latest?database=testdb")
	if msg, _ := body["error"].(string); code != http.StatusNotFound ||
		!strings.Contains(msg, "no briefing yet") {
		t.Fatalf("before any briefing: status %d body %v, want 404 no briefing yet", code, body)
	}
	w := briefing.New(pool, &config.Config{}, nil, func(string, string, ...any) {}).
		WithDatabaseName("testdb")
	r, err := w.GenerateBriefing(ctx)
	if err != nil || r.ID == 0 {
		t.Fatalf("GenerateBriefing: id %d err %v", r.ID, err)
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT content_json::text FROM sage.briefings WHERE id = $1",
		r.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored content_json: %v", err)
	}
	code, body = getBriefings(t, pool, "/api/v1/briefings/latest?database=testdb")
	if code != http.StatusOK {
		t.Fatalf("latest: status %d body %v", code, body)
	}
	rows := briefingRows(t, body)
	if len(rows) != 1 || rows[0]["database"] != "testdb" || rows[0]["id"] != float64(r.ID) {
		t.Fatalf("latest rows = %v, want testdb's briefing %d", rows, r.ID)
	}
	content, _ := json.Marshal(rows[0]["content_json"])
	assertSameJSON(t, string(content), stored)
	if !strings.Contains(string(content), `"window"`) || rows[0]["delivery_status"] != nil {
		t.Fatalf("content %s delivery %v", content, rows[0]["delivery_status"])
	}
	code, body = getBriefings(t, pool, "/api/v1/briefings?limit=5")
	if rows := briefingRows(t, body); code != http.StatusOK || len(rows) != 1 ||
		rows[0]["content_text"] != r.Text {
		t.Fatalf("list: status %d rows %v", code, rows)
	}
}
