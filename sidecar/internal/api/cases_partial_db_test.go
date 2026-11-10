package api

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// queryProjectedCases is the cases of the selected databases and the first
// database's read error, for tests that predate the partial report.
func queryProjectedCases(
	ctx context.Context, mgr *fleet.DatabaseManager, database string,
) ([]cases.Case, error) {
	report := collectCases(ctx, mgr, database)
	return report.Cases, report.firstErr()
}

// poolManager registers each name on its pool.
func poolManager(pools map[string]*pgxpool.Pool) *fleet.DatabaseManager {
	mgr := fleet.NewManager(caseAutonomousConfig())
	for name, pool := range pools {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Config: config.DatabaseConfig{Name: name}, Pool: pool,
		})
	}
	return mgr
}

func seedPartialFindings(t *testing.T, ctx context.Context, n int, severity string) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, status)
		SELECT 'stale_statistics', $2, 'table', 'public.' || $2 || g,
		       'finding ' || g, '{}'::jsonb, 'analyze it', 'open'
		  FROM generate_series(1, $1) AS g`, n, severity)
	if err != nil {
		t.Fatalf("seed %d %s findings: %v", n, severity, err)
	}
}

func seedPartialIncidents(t *testing.T, ctx context.Context, n int, severity string) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, signal_ids, database_name, detected_at)
		SELECT $2, 'cause ' || g, 'deterministic', ARRAY['sig' || g], 'prod',
		       now() - g * interval '1 minute'
		  FROM generate_series(1, $1) AS g`, n, severity)
	if err != nil {
		t.Fatalf("seed %d incidents: %v", n, err)
	}
}

func seedPartialHints(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status)
		SELECT 900000 + g, 'SeqScan(t' || g || ')', 'slow', 'active'
		  FROM generate_series(1, $1) AS g`, n)
	if err != nil {
		t.Fatalf("seed %d hints: %v", n, err)
	}
}

// CHECK-09: one database whose pool is closed leaves the fleet request
// answering 200 with the other database's cases.
func TestCasesClosedPoolFleetAnswersPartial(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedPartialFindings(t, ctx, 3, "warning")
	closed, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	closed.Close()
	mgr := poolManager(map[string]*pgxpool.Pool{"alpha": pool, "beta": closed})

	code, body := getCases(t, mgr, "")

	if code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(body.Cases) != 3 {
		t.Fatalf("cases = %d, want alpha's 3", len(body.Cases))
	}
	for _, c := range body.Cases {
		if c.DatabaseName != "alpha" {
			t.Fatalf("case %s from %q, want alpha only", c.ID, c.DatabaseName)
		}
	}
	want := casesUnavailable{Database: "beta", ErrorClass: caseErrorConnection}
	if len(body.Unavailable) != 1 || body.Unavailable[0] != want {
		t.Fatalf("unavailable = %+v, want [%+v]", body.Unavailable, want)
	}

	code, body = getCases(t, mgr, "?database=beta")
	if code != 503 || body.ErrorClass != caseErrorConnection {
		t.Fatalf("beta alone = %d %+v, want 503 connection", code, body)
	}
}

// CHECK-10: each source reports truncated exactly when it holds one row
// more than its cap.
func TestCasesTruncatedFlipsAtCapPlusOne(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	sources := []struct {
		name string
		cap  int
		seed func(n int)
		cut  func(casesTruncation) bool
	}{
		{"findings", caseFindingsCap,
			func(n int) { seedPartialFindings(t, ctx, n, "warning") },
			func(c casesTruncation) bool { return c.Findings }},
		{"incidents", caseIncidentsCap,
			func(n int) { seedPartialIncidents(t, ctx, n, "warning") },
			func(c casesTruncation) bool { return c.Incidents }},
		{"query_hints", caseQueryHintsCap, func(n int) { seedPartialHints(t, ctx, n) },
			func(c casesTruncation) bool { return c.QueryHints }},
	}
	for _, src := range sources {
		for _, n := range []int{src.cap, src.cap + 1} {
			resetCaseTables(t, ctx)
			src.seed(n)
			code, body := getCases(t, mgr, "?database=prod")
			if code != 200 {
				t.Fatalf("%s n=%d status = %d", src.name, n, code)
			}
			cut := src.cut(body.Truncated["prod"])
			if cut != (n > src.cap) {
				t.Fatalf("%s n=%d truncated = %v (%+v), want %v",
					src.name, n, cut, body.Truncated, n > src.cap)
			}
			if body.Total != src.cap {
				t.Fatalf("%s n=%d total = %d, want the cap %d", src.name, n,
					body.Total, src.cap)
			}
		}
	}
}

// CHECK-11: a focused case the cap dropped is read from its database.
func TestCasesFocusLoadsDroppedFinding(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedPartialFindings(t, ctx, caseFindingsCap, "critical")
	seedPartialFindings(t, ctx, 1, "info")
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	dropped := "finding:prod:stale_statistics:table:public.info1"

	_, plain := getCases(t, mgr, "?database=prod")
	if containsCase(plain.Cases, dropped) {
		t.Fatalf("the info finding is on the capped page; the test needs it dropped")
	}

	code, body := getCases(t, mgr, "?database=prod&case="+url.QueryEscape(dropped))
	if code != 200 || body.Focus == nil || !body.Focus.Found || !body.Focus.Loaded {
		t.Fatalf("focus = %d %+v, want found and loaded", code, body.Focus)
	}
	if !containsCase(body.Cases, dropped) || body.Total != caseFindingsCap+1 {
		t.Fatalf("cases total %d, focused present %v; want %d with the focus",
			body.Total, containsCase(body.Cases, dropped), caseFindingsCap+1)
	}

	_, missing := getCases(t, mgr, "?case="+url.QueryEscape(dropped+"x"))
	if missing.Focus == nil || missing.Focus.Found {
		t.Fatalf("unknown focus = %+v, want not found", missing.Focus)
	}
}

func TestCasesFocusLoadsDroppedIncident(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedPartialIncidents(t, ctx, caseIncidentsCap, "critical")
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, signal_ids, database_name, detected_at)
		VALUES ('info', 'old cause', 'deterministic', ARRAY['lonely'], 'prod',
		        now() - interval '9 days')
		RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("seed dropped incident: %v", err)
	}
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	dropped := fmt.Sprintf("incident:prod:lonely:%s", id)

	_, plain := getCases(t, mgr, "")
	if containsCase(plain.Cases, dropped) || !plain.Truncated["prod"].Incidents {
		t.Fatalf("dropped incident on page %v, truncated %+v; want dropped and cut",
			containsCase(plain.Cases, dropped), plain.Truncated)
	}
	_, body := getCases(t, mgr, "?case="+url.QueryEscape(dropped))
	if body.Focus == nil || !body.Focus.Loaded || !containsCase(body.Cases, dropped) {
		t.Fatalf("focus = %+v, want the dropped incident loaded", body.Focus)
	}
}

// With sage.action_log unreadable the finding cases say their actions are
// unavailable instead of reading as "no actions".
func TestCasesActionLogUnreadableMarksActionsUnavailable(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedPartialFindings(t, ctx, 2, "warning")
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})

	_, before := getCases(t, mgr, "")
	for _, c := range before.Cases {
		if c.ActionsUnavailable {
			t.Fatalf("case %s actions_unavailable with a readable log", c.ID)
		}
	}

	if _, err := pool.Exec(ctx,
		`ALTER TABLE sage.action_log RENAME TO action_log_hidden_cases05`); err != nil {
		t.Fatalf("hide action_log: %v", err)
	}
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(),
			`ALTER TABLE sage.action_log_hidden_cases05 RENAME TO action_log`)
		if err != nil {
			t.Errorf("restore action_log: %v", err)
		}
	})

	code, after := getCases(t, mgr, "")
	if code != 200 || len(after.Cases) != 2 {
		t.Fatalf("status %d, %d cases; want 200 with both findings", code, len(after.Cases))
	}
	for _, c := range after.Cases {
		if !c.ActionsUnavailable {
			t.Fatalf("case %s actions_unavailable = false with action_log unreadable", c.ID)
		}
	}
}

// Concurrent access on real pools: four databases give the same order on
// every request.
func TestCasesFourDatabasesKeepOneOrder(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedPartialFindings(t, ctx, 5, "warning")
	pools := map[string]*pgxpool.Pool{}
	for _, name := range []string{"d1", "d2", "d3", "d4"} {
		pools[name] = pool
	}
	mgr := poolManager(pools)
	var first string
	for run := 0; run < 4; run++ {
		got, err := queryProjectedCases(ctx, mgr, "all")
		if err != nil || len(got) != 20 {
			t.Fatalf("run %d: %d cases, err %v; want 20", run, len(got), err)
		}
		order := strings.Join(caseIDs(got), ",")
		if run == 0 {
			first = order
		} else if order != first {
			t.Fatalf("run %d order differs", run)
		}
	}
}

func containsCase(list []cases.Case, id string) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

// A source table that cannot be read makes its database unavailable; the
// read must not pass for an empty one. Once pgx has cached the statement it
// reports a missing table from rows.Err, not from Query, so the scanners
// must check it; the first request warms that cache.
func TestCasesUnreadableSourceMarksDatabaseUnavailable(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	for _, table := range []string{"incidents", "query_hints"} {
		t.Run(table, func(t *testing.T) {
			resetCaseTables(t, ctx)
			seedPartialFindings(t, ctx, 2, "warning")
			mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
			if code, body := getCases(t, mgr, ""); code != 200 || len(body.Cases) != 2 {
				t.Fatalf("warm-up = %d with %d cases, want 200 with 2", code, len(body.Cases))
			}
			hidden := table + "_hidden_cases05"
			if _, err := pool.Exec(ctx,
				`ALTER TABLE sage.`+table+` RENAME TO `+hidden); err != nil {
				t.Fatalf("hide %s: %v", table, err)
			}
			t.Cleanup(func() {
				if _, err := pool.Exec(context.Background(),
					`ALTER TABLE sage.`+hidden+` RENAME TO `+table); err != nil {
					t.Errorf("restore %s: %v", table, err)
				}
			})

			code, body := getCases(t, mgr, "")
			want := casesUnavailable{Database: "prod", ErrorClass: caseErrorQuery}
			if code != 200 || len(body.Unavailable) != 1 || body.Unavailable[0] != want {
				t.Fatalf("fleet = %d %+v, want 200 with [%+v]", code, body.Unavailable, want)
			}
			if len(body.Cases) != 0 {
				t.Fatalf("cases = %d, want none from a half-read database", len(body.Cases))
			}
			if code, body = getCases(t, mgr, "?database=prod"); code != 503 ||
				body.ErrorClass != caseErrorQuery {
				t.Fatalf("single = %d %+v, want 503 query", code, body)
			}
		})
	}
}
