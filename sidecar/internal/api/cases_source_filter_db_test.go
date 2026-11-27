package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// rules-12b on real PostgreSQL: ?finding_source= filters the open
// findings in SQL, before the 500-row cap, and leaves out incidents and
// query hints.

// seedSourceFinding inserts one open finding and returns its id.
func seedSourceFinding(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, category, severity, object string,
) string {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, status)
		VALUES ($1, $2, 'table', $3, $1 || ' on ' || $3, '{}'::jsonb, 'look', 'open')
		RETURNING id`, category, severity, object).Scan(&id)
	if err != nil {
		t.Fatalf("seed %s finding: %v", category, err)
	}
	return strconv.FormatInt(id, 10)
}

// sourceFixture seeds one optimizer, one analyzer (rules) and one
// forecaster finding, plus an incident and a query hint.
func sourceFixture(t *testing.T) (*pgxpool.Pool, context.Context, map[string]string) {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.query_hints"); err != nil {
		t.Fatalf("clean hints: %v", err)
	}
	ids := map[string]string{
		"optimizer": seedSourceFinding(t, ctx, pool, "missing_index", "warning", "public.orders"),
		"rules":     seedSourceFinding(t, ctx, pool, "unused_index", "warning", "public.items"),
		"forecaster": seedSourceFinding(t, ctx, pool,
			"forecast_disk_growth", "warning", "public.big"),
	}
	seedPartialIncidents(t, ctx, 1, "warning")
	seedPartialHints(t, ctx, 1)
	return pool, ctx, ids
}

// findingIDsOf is the source ids of a response's cases, failing on any
// case that is not finding-backed.
func findingIDsOf(t *testing.T, body casesWire) []string {
	t.Helper()
	var out []string
	for _, c := range body.Cases {
		if strings.HasPrefix(c.ID, "incident:") || strings.HasPrefix(c.ID, "query_hint:") {
			t.Fatalf("case %s is not finding-backed", c.ID)
		}
		out = append(out, c.SourceIDs...)
	}
	return out
}

func TestCasesSourceFilter_DBReturnsOnlyThatSubsystem(t *testing.T) {
	pool, _, ids := sourceFixture(t)
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})

	code, all := getCases(t, mgr, "?database=prod")
	if code != 200 || len(all.Cases) != 5 {
		t.Fatalf("unfiltered = %d %v, want 5 cases (3 findings, incident, hint)",
			code, caseIDs(all.Cases))
	}
	for _, src := range []string{"optimizer", "rules", "forecaster"} {
		code, body := getCases(t, mgr, "?database=prod&finding_source="+src)
		got := findingIDsOf(t, body)
		if code != 200 || len(got) != 1 || got[0] != ids[src] {
			t.Fatalf("%s: %d source ids %v, want exactly [%s]", src, code, got, ids[src])
		}
	}
}

func TestCasesSourceFilter_DBReservedSourceMatchesNothing(t *testing.T) {
	pool, _, _ := sourceFixture(t)
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	body := getCasesBody(t, mgr, "?database=prod&finding_source=migration_advisor")
	list, ok := body["cases"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("cases = %#v, want an empty list", body["cases"])
	}
	f, _ := body["filtered"].(map[string]any)
	if f["finding_source"] != "migration_advisor" {
		t.Fatalf("filtered = %#v, want migration_advisor", body["filtered"])
	}
}

func TestCasesSourceFilter_DBEmptyValueIsByteEqualToNoFilter(t *testing.T) {
	pool, _, _ := sourceFixture(t)
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	read := func(query string) []byte {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cases"+query, nil)
		rec := httptest.NewRecorder()
		casesHandler(mgr).ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%q: status %d %s", query, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	plain, empty := read("?database=prod"), read("?database=prod&finding_source=")
	if !bytes.Equal(plain, empty) {
		t.Fatalf("finding_source= differs from no filter:\n%s\n%s", plain, empty)
	}
	if !bytes.Contains(plain, []byte(`"finding_sources":["`)) {
		t.Fatalf("response lacks a finding_sources list: %s", plain)
	}
}

func TestCasesSourceFilter_DBInvalidSourceRunsNoSQL(t *testing.T) {
	pool, _, _ := sourceFixture(t)
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for _, raw := range []string{"nope", "analyzer", "optimizer'--"} {
		code, body := getCases(t, mgr, "?database=prod&finding_source="+url.QueryEscape(raw))
		if code != http.StatusBadRequest || body.Error == "" {
			t.Fatalf("%q: %d %+v, want 400 with an error", raw, code, body)
		}
	}
	for _, bad := range []string{"SQLSTATE", "syntax error", "cases read failed"} {
		if strings.Contains(logs.String(), bad) {
			t.Fatalf("log carries %q:\n%s", bad, logs.String())
		}
	}
}

// 600 warning analyzer findings sort ahead of one info optimizer finding,
// so the unfiltered list cuts it; the filter applies before the cap.
func TestCasesSourceFilter_DBFilterAppliesBeforeCap(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedPartialFindings(t, ctx, caseFindingsCap+100, "warning")
	optID := seedSourceFinding(t, ctx, pool, "missing_index", "info", "public.late")
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})

	_, all := getCases(t, mgr, "?database=prod")
	if !all.Truncated["prod"].Findings {
		t.Fatalf("unfiltered truncated = %+v, want findings cut", all.Truncated)
	}
	for _, id := range findingIDsOf(t, all) {
		if id == optID {
			t.Fatal("the info optimizer finding fits the unfiltered cap; fixture is wrong")
		}
	}
	code, body := getCases(t, mgr, "?database=prod&finding_source=optimizer")
	got := findingIDsOf(t, body)
	if code != 200 || len(got) != 1 || got[0] != optID {
		t.Fatalf("filtered = %d %v, want [%s]", code, got, optID)
	}
	if body.Truncated["prod"].Findings {
		t.Fatalf("filtered truncated = %+v, want not cut", body.Truncated)
	}
}

// A database that cannot be read answers 503 naming it, never an empty
// 200 that would read as "no optimizer findings".
func TestCasesSourceFilter_DBClosedPoolIs503(t *testing.T) {
	_, ctx := phase2RequireDB(t)
	closed, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	closed.Close()
	mgr := poolManager(map[string]*pgxpool.Pool{"beta": closed})
	code, body := getCases(t, mgr, "?database=beta&finding_source=optimizer")
	if code != http.StatusServiceUnavailable || body.Database != "beta" ||
		!strings.Contains(body.Error, "beta") || len(body.Cases) != 0 {
		t.Fatalf("closed pool = %d %+v, want 503 naming beta", code, body)
	}
}

// Requests with different sources share one pool and each gets its own
// rows.
func TestCasesSourceFilter_DBConcurrentSources(t *testing.T) {
	pool, _, ids := sourceFixture(t)
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	handler := casesHandler(mgr)
	sources := []string{"optimizer", "rules", "forecaster", "optimizer"}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := sources[i%len(sources)]
			if err := checkSourceRequest(handler, src, ids[src]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func checkSourceRequest(h http.Handler, src, wantID string) error {
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/cases?database=prod&finding_source="+src, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body casesWire
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return fmt.Errorf("%s: decode %s: %w", src, rec.Body, err)
	}
	if rec.Code != 200 || len(body.Cases) != 1 ||
		len(body.Cases[0].SourceIDs) != 1 || body.Cases[0].SourceIDs[0] != wantID {
		return fmt.Errorf("%s: %d %v, want only finding %s", src, rec.Code,
			caseIDs(body.Cases), wantID)
	}
	return nil
}
