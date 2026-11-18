package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// evid-03b: GET /api/v1/databases/{db}/facts/peak-suggestion computes the
// peak-window suggestion on request (nothing is stored); the operator
// applies it as an ordinary declaration of the owner brief. Every
// signed-in role reads it; a database the caller cannot read is refused
// by the route's read scope (route_scopes.golden).
// No concurrent access tests: the route only reads.

const peakPath = "/api/v1/databases/testdb/facts/peak-suggestion"

// seedMondayPeaks fills 29 days of coverage and hourly rollup ending at the
// current hour, with Monday 09:00-12:00 UTC busiest every week.
func seedMondayPeaks(t *testing.T, f *factAPI) {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		_, _ = f.pool.Exec(ctx, "DELETE FROM sage.query_rollup_hourly")
		_, _ = f.pool.Exec(ctx, "DELETE FROM sage.query_rollup_coverage")
	}
	clean()
	t.Cleanup(clean)
	end := time.Now().UTC().Truncate(time.Hour).Add(time.Hour)
	start := end.Add(-29 * 24 * time.Hour)
	if _, err := f.pool.Exec(ctx, `INSERT INTO sage.query_rollup_coverage (database_id,
		hour, db_exec_ms, sampled_exec_ms, sampled_queryids, epoch_breaks, snapshots,
		epoch_source)
		SELECT NULL, g, 200, 150, 1, 0, 12, 'recorded' FROM generate_series($1::timestamptz,
		$2::timestamptz - interval '1 hour', interval '1 hour') g`, start, end); err != nil {
		t.Fatalf("seed coverage: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (database_id,
		queryid, hour, calls, exec_ms, rows, shared_blks_hit, shared_blks_read,
		temp_blks_read, temp_blks_written, epoch_break, pairs)
		SELECT NULL, 1, g, 1, CASE WHEN extract(isodow FROM g AT TIME ZONE 'UTC') = 1
		  AND extract(hour FROM g AT TIME ZONE 'UTC') BETWEEN 9 AND 11 THEN 10000 ELSE 100 END,
		  1, 0, 0, 0, 0, false, 12
		FROM generate_series($1::timestamptz, $2::timestamptz - interval '1 hour',
		interval '1 hour') g`, start, end); err != nil {
		t.Fatalf("seed hourly rollup: %v", err)
	}
}

func TestFactPeakSuggestion_NoneWithItsReason(t *testing.T) {
	f := newFactAPI(t)
	ctx := context.Background()
	_, _ = f.pool.Exec(ctx, "DELETE FROM sage.query_rollup_hourly")
	_, _ = f.pool.Exec(ctx, "DELETE FROM sage.query_rollup_coverage")
	code, body := f.do(t, viewerUser(), "GET", peakPath, nil)
	reason, _ := body["reason"].(string)
	if code != http.StatusOK || body["suggestion"] != "none" || reason == "" ||
		body["database"] != "testdb" || body["peak_windows"] != nil {
		t.Fatalf("no rollup: %d %v", code, body)
	}
	if hours, ok := body["hours"].([]any); !ok || len(hours) != 0 {
		t.Fatalf("hours = %#v, want an empty list", body["hours"])
	}
}

func TestFactPeakSuggestion_SuggestsTheMeasuredPeak(t *testing.T) {
	f := newFactAPI(t)
	seedMondayPeaks(t, f)
	code, body := f.do(t, viewerUser(), "GET", peakPath, nil)
	if code != http.StatusOK || body["suggestion"] != "peak_windows" ||
		body["peak_windows"] != "mon 09:00-12:00 UTC" || body["coverage_status"] != "ok" {
		t.Fatalf("suggestion: %d %v", code, body)
	}
	if hours, _ := body["hours"].([]any); len(hours) < 9 {
		t.Fatalf("hours it rests on = %d, want at least 9", len(hours))
	}
	// Applying it is an ordinary declaration of the brief.
	code, body = f.do(t, testOperatorUser(), "POST", "/api/v1/facts?database=testdb",
		briefBody(map[string]string{"owner": "Payments",
			"peak_windows": "mon 09:00-12:00 UTC"}))
	if code != http.StatusCreated {
		t.Fatalf("apply suggestion: %d %v", code, body)
	}
	// Nothing was stored by asking: only the declared brief exists.
	var n int
	if err := f.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.facts").Scan(&n); err != nil || n != 1 {
		t.Fatalf("facts stored = %d (%v), want the one declared brief", n, err)
	}
}

func TestFactPeakSuggestion_RefusesBadDatabases(t *testing.T) {
	f := newFactAPI(t)
	for path, want := range map[string]int{
		"/api/v1/databases/nosuch/facts/peak-suggestion": http.StatusNotFound,
		"/api/v1/databases/all/facts/peak-suggestion":    http.StatusBadRequest,
	} {
		code, body := f.do(t, viewerUser(), "GET", path, nil)
		if code != want {
			t.Errorf("%s: %d %v, want %d", path, code, body, want)
		}
	}
	code, body := f.do(t, nil, "GET", peakPath, nil)
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("anonymous read: %d %v", code, body)
	}
	if code, body := f.do(t, viewerUser(), "POST", peakPath, nil); code == http.StatusOK ||
		strings.Contains(strings.ToLower(asString(body["suggestion"])), "peak") {
		t.Fatalf("POST answered: %d %v", code, body)
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
