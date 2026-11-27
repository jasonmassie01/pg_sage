package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// The workload explorer routes (market-01) without a database: request
// validation, the empty answers and the merge of statement texts.

func workloadMux(mgr *fleet.DatabaseManager) *http.ServeMux {
	mux := http.NewServeMux()
	registerWorkloadRoutes(mux, mgr, nil)
	return mux
}

func getWorkload(t *testing.T, mux *http.ServeMux, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(httptest.NewRequest(http.MethodGet, path, nil), viewerUser()))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: body %q is not JSON: %v", path, w.Body.String(), err)
	}
	return w.Code, body
}

// noPoolMgr has one database with no pool (not connected yet) and a
// second one, so database=all is a fleet.
func noPoolMgr() *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, name := range []string{"alpha", "beta"} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: false}})
	}
	return mgr
}

func TestQueriesTop_InvalidInputIs400(t *testing.T) {
	mux := workloadMux(noPoolMgr())
	since := url.QueryEscape("2026-11-10T10:00:00Z")
	until := url.QueryEscape("2026-11-10T09:00:00Z")
	cases := map[string]string{
		"limit zero":     "limit=0",
		"limit 101":      "limit=101",
		"limit text":     "limit=ten",
		"unknown order":  "order=total_time",
		"until before":   "since=" + since + "&until=" + until,
		"bad since":      "since=yesterday",
		"bad until":      "until=1731000000",
		"window past 7d": "since=2026-10-01T00:00:00Z&until=2026-11-10T00:00:00Z",
	}
	for name, query := range cases {
		cases[name] = "database=alpha&" + query
	}
	cases["bad database name"] = "database=a%3Bdrop"
	for name, query := range cases {
		code, body := getWorkload(t, mux, "/api/v1/queries/top?"+query)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status %d (%v), want 400", name, code, body)
			continue
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Errorf("%s: 400 without an error message: %v", name, body)
		}
	}
}

func TestQueriesTop_LimitBoundsAccepted(t *testing.T) {
	mux := workloadMux(noPoolMgr())
	for _, limit := range []string{"1", "100"} {
		code, body := getWorkload(t, mux, "/api/v1/queries/top?database=alpha&limit="+limit)
		if code != http.StatusOK {
			t.Errorf("limit %s: status %d (%v), want 200", limit, code, body)
		}
	}
}

// No samples: an empty list and coverage none, never a row of zeros.
func TestQueriesTop_NoPoolIsCoverageNone(t *testing.T) {
	mux := workloadMux(noPoolMgr())
	code, body := getWorkload(t, mux, "/api/v1/queries/top?database=alpha")
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	queries, ok := body["queries"].([]any)
	if !ok || len(queries) != 0 {
		t.Fatalf("queries = %#v, want an empty JSON list", body["queries"])
	}
	if body["coverage"] != querystore.CoverageNone || body["database"] != "alpha" ||
		body["order"] != "load" {
		t.Fatalf("body %v, want coverage none for alpha ordered by load", body)
	}
}

func TestQueriesTop_UnknownDatabaseIs404(t *testing.T) {
	code, body := getWorkload(t, workloadMux(noPoolMgr()),
		"/api/v1/queries/top?database=gamma")
	if code != http.StatusNotFound {
		t.Fatalf("status %d (%v), want 404", code, body)
	}
}

func TestQueryDetail_InvalidInputIs400(t *testing.T) {
	mux := workloadMux(noPoolMgr())
	cases := map[string]string{
		"queryid text":     "/api/v1/queries/abc?database=alpha",
		"queryid overflow": "/api/v1/queries/99999999999999999999?database=alpha",
		"no database":      "/api/v1/queries/42",
		"database all":     "/api/v1/queries/42?database=all",
		"until before":     "/api/v1/queries/42?database=alpha&since=2026-11-10T10:00:00Z&until=2026-11-10T09:00:00Z",
	}
	for name, path := range cases {
		code, body := getWorkload(t, mux, path)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status %d (%v), want 400", name, code, body)
		}
	}
}

func TestQueryDetail_NoPoolIsCoverageNone(t *testing.T) {
	code, body := getWorkload(t, workloadMux(noPoolMgr()),
		"/api/v1/queries/-9223372036854775808?database=alpha")
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	if body["queryid"] != "-9223372036854775808" || body["coverage"] != "none" {
		t.Fatalf("body %v, want the exact queryid as a string and coverage none", body)
	}
	for _, key := range []string{"buckets", "plan_changes", "cases", "hints", "actions"} {
		if list, ok := body[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %#v, want an empty list", key, body[key])
		}
	}
}

func TestWaitLoad_InvalidInputIs400(t *testing.T) {
	mux := workloadMux(noPoolMgr())
	for name, path := range map[string]string{
		"database all": "/api/v1/wait-load?database=all",
		"no database":  "/api/v1/wait-load",
		"bad since":    "/api/v1/wait-load?database=alpha&since=noon",
	} {
		if code, body := getWorkload(t, mux, path); code != http.StatusBadRequest {
			t.Errorf("%s: status %d (%v), want 400", name, code, body)
		}
	}
}

func TestWaitLoad_NoPoolIsCoverageNone(t *testing.T) {
	code, body := getWorkload(t, workloadMux(noPoolMgr()), "/api/v1/wait-load?database=alpha")
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	if body["coverage"] != "none" || body["source"] != waitSourceActivity ||
		!strings.Contains(fmt.Sprint(body["sampling"]), "sampled each collector cycle") {
		t.Fatalf("body %v, want coverage none from pg_stat_activity sampled each cycle", body)
	}
	if series, ok := body["series"].([]any); !ok || len(series) != 0 {
		t.Fatalf("series = %#v, want an empty list", body["series"])
	}
}

func TestParseWindow_DefaultsAndBoundary(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	from, to, err := parseWindow(url.Values{}, now, time.Hour)
	if err != nil || !to.Equal(now) || !from.Equal(now.Add(-time.Hour)) {
		t.Fatalf("defaults = %v..%v (%v), want the last hour", from, to, err)
	}
	q := url.Values{"since": {now.Add(-maxWorkloadWindow).Format(time.RFC3339)},
		"until": {now.Format(time.RFC3339)}}
	if _, _, err := parseWindow(q, now, time.Hour); err != nil {
		t.Errorf("a window of exactly %v refused: %v", maxWorkloadWindow, err)
	}
	q.Set("since", now.Add(-maxWorkloadWindow-time.Second).Format(time.RFC3339))
	if _, _, err := parseWindow(q, now, time.Hour); err == nil {
		t.Error("a window one second past the bound accepted")
	}
}

// Error propagation: without pg_stat_statements the ranked series stays
// and each text is replaced by why it is missing.
func TestMergeTopTexts_StatementsUnavailableKeepsSeries(t *testing.T) {
	res := querystore.TopResult{Coverage: querystore.CoverageFull, Window: time.Hour,
		Queries: []querystore.WindowStat{{QueryID: 11, Intervals: 1, Calls: 3,
			TotalMs: 3600}}}
	err := fmt.Errorf("%w: pg_stat_statements is not usable here: relation does not exist",
		agenttools.ErrUnavailable)
	rows, excluded, reason := mergeTopTexts("alpha", res, nil, err, 5)
	if len(rows) != 1 || rows[0].QueryID != "11" || rows[0].Query != "" ||
		rows[0].Load != 0.001 || excluded != 0 {
		t.Fatalf("rows %+v excluded %d, want the series row kept without text", rows, excluded)
	}
	if !strings.Contains(reason, "pg_stat_statements") ||
		rows[0].TextUnavailable != reason {
		t.Fatalf("reason %q on row %q, want the pg_stat_statements reason on both",
			reason, rows[0].TextUnavailable)
	}
}

func TestMergeTopTexts_ExcludesPgSageStatementsAndCuts(t *testing.T) {
	res := querystore.TopResult{Window: time.Minute, Queries: []querystore.WindowStat{
		{QueryID: 1, Intervals: 1, TotalMs: 900}, {QueryID: 2, Intervals: 1, TotalMs: 800},
		{QueryID: 3, Intervals: 1, TotalMs: 700}, {QueryID: 4, Intervals: 1, TotalMs: 600},
	}}
	texts := map[int64]agenttools.Statement{
		1: {Query: "/* pg_sage */ SELECT 1", Excluded: true},
		2: {Query: "SELECT * FROM orders WHERE id = $1"},
		3: {Query: "UPDATE orders SET paid = $1"},
	}
	rows, excluded, reason := mergeTopTexts("alpha", res, texts, nil, 2)
	if reason != "" || excluded != 1 || len(rows) != 2 {
		t.Fatalf("rows %+v excluded %d reason %q, want 2 rows and 1 excluded", rows,
			excluded, reason)
	}
	if rows[0].QueryID != "2" || rows[1].QueryID != "3" || rows[0].Database != "alpha" ||
		rows[0].Query != texts[2].Query {
		t.Fatalf("rows %+v, want queries 2 and 3 of alpha with their text", rows)
	}
	gone := map[int64]agenttools.Statement{}
	rows, _, _ = mergeTopTexts("alpha", res, gone, nil, 1)
	if rows[0].Query != "" || !strings.Contains(rows[0].TextUnavailable, "evicted") {
		t.Fatalf("row %+v, want an evicted statement explained", rows[0])
	}
}

func TestStatementsError_NotUnavailableIsAnError(t *testing.T) {
	if r := textUnavailableReason(errors.New("connection reset")); r != "" {
		t.Errorf("a transport error read as a missing extension: %q", r)
	}
}
