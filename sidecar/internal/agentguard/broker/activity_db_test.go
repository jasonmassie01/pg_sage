//go:build cgo

package broker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G1-10: each statement a broker role runs appears in the principal's
// activity, attributed by pg_stat_statements userid; when attribution may
// have been lost the view says so and falls back to guard_query_audit.

func ensurePSS(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	require.NoError(t, err)
}

func (f *fixture) activity(t *testing.T) Activity {
	t.Helper()
	a, err := ReadActivity(context.Background(), f.target, ActivityRequest{PrincipalID: f.p.ID,
		From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute), Limit: 50})
	require.NoError(t, err)
	return a
}

// runTagged runs n distinct agent queries, each with an alias naming it.
// pg_stat_statements ignores aliases when it groups statements, so each
// query also selects a different number of columns: n separate entries.
func (f *fixture) runTagged(t *testing.T, n int) []string {
	t.Helper()
	var tags []string
	for i := 0; i < n; i++ {
		tag := fmt.Sprintf("g110_%s_%d", f.schema[3:], i)
		extra := strings.Repeat(", id", i)
		res := f.query(t, fmt.Sprintf("SELECT id AS %s%s FROM items WHERE id = $1", tag,
			extra), "1")
		require.True(t, res.Status == StatusOK, "query %d: %+v", i, res)
		tags = append(tags, tag)
	}
	return tags
}

func TestActivityAttributesEveryStatementByRole(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	ensurePSS(t, f.super)
	tags := f.runTagged(t, 3)
	blocked := f.query(t, "SELECT pg_sleep(0)")
	require.True(t, blocked.Verdict == VerdictBlocked, "pg_sleep: %+v", blocked)

	a := f.activity(t)
	if a.Attribution.Source != SourcePSS || !a.Attribution.Complete || a.Attribution.Dropped {
		t.Fatalf("attribution = %+v, want complete from pg_stat_statements", a.Attribution)
	}
	for _, tag := range tags {
		found := false
		for _, s := range a.Statements {
			if strings.Contains(s.Query, tag) {
				found = true
				if s.Role != f.role || s.Calls < 1 {
					t.Errorf("statement %q attributed to %s with %d calls", tag, s.Role, s.Calls)
				}
			}
		}
		if !found {
			t.Errorf("statement %s is missing from the principal's activity", tag)
		}
	}
	for _, s := range a.Statements {
		if s.Role != f.role {
			t.Errorf("activity lists another role's statement: %+v", s)
		}
	}
	if len(a.Queries) != 4 || a.Queries[0].Verdict != VerdictBlocked {
		t.Errorf("audit entries = %+v, want 4, newest (blocked) first", a.Queries)
	}
	if a.Attribution.AuditedExecutions != 3 || a.Attribution.AttributedCalls < 3 {
		t.Errorf("attribution counts = %+v, want 3 audited, >= 3 attributed", a.Attribution)
	}
}

// Entries removed for the role (as a partial reset or an eviction would)
// are detected by the coverage check, and the view falls back to the audit.
func TestActivityFallsBackWhenEntriesAreMissing(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	ensurePSS(t, f.super)
	f.runTagged(t, 2)
	exec(t, f.super, fmt.Sprintf("SELECT pg_stat_statements_reset(to_regrole('%s')::oid, "+
		"0, 0)", f.role))
	a := f.activity(t)
	if !a.Attribution.Dropped || a.Attribution.Source != SourceAudit ||
		a.Attribution.Reason != ReasonEntriesMissing {
		t.Fatalf("attribution = %+v, want dropped (entries_missing) from the audit",
			a.Attribution)
	}
	if len(a.Queries) != 2 {
		t.Errorf("fallback audit entries = %d, want 2", len(a.Queries))
	}
}

func TestActivityPaging(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	ensurePSS(t, f.super)
	f.runTagged(t, 3)
	req := ActivityRequest{PrincipalID: f.p.ID, From: time.Now().Add(-time.Hour),
		To: time.Now().Add(time.Minute), Limit: 2}
	a, err := ReadActivity(context.Background(), f.target, req)
	require.NoError(t, err)
	if len(a.Queries) != 2 || a.NextCursor == "" {
		t.Fatalf("page 1 = %d entries, cursor %q", len(a.Queries), a.NextCursor)
	}
	req.Cursor = a.NextCursor
	b, err := ReadActivity(context.Background(), f.target, req)
	require.NoError(t, err)
	if len(b.Queries) != 1 || b.NextCursor != "" || b.Queries[0].ID >= a.Queries[1].ID {
		t.Errorf("page 2 = %+v, cursor %q", b.Queries, b.NextCursor)
	}
	req.Cursor = "not-a-cursor"
	if _, err := ReadActivity(context.Background(), f.target, req); err == nil {
		t.Error("a malformed cursor was accepted")
	}
	req.Cursor, req.Limit = "", 0
	if _, err := ReadActivity(context.Background(), f.target, req); err == nil {
		t.Error("limit 0 was accepted")
	}
}

// The dealloc arm needs a server whose pg_stat_statements.max is small
// enough to evict on demand (the shared test servers keep 50000 entries
// so other suites' probes survive). SAGE_TEST_PSS_SMALL_URL names one,
// e.g. a container started with -c pg_stat_statements.max=100.
func TestActivityReportsDeallocAndFallsBack(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_PSS_SMALL_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_PSS_SMALL_URL not set: no server with a small " +
			"pg_stat_statements.max to evict on")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, schema.Bootstrap(ctx, pool))
	ensurePSS(t, pool)
	f := newFixtureOn(t, pool, envbind.EnvProd)
	// Bootstrap and the fixture alone overflow 100 entries; start the
	// attribution window from a clean slate (this server is the test's own).
	exec(t, pool, "SELECT pg_stat_statements_reset()")
	f.runTagged(t, 2)
	if a := f.activity(t); a.Attribution.Dropped {
		t.Fatalf("before eviction: %+v, want complete", a.Attribution)
	}
	var max int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT current_setting('pg_stat_statements.max')::int").Scan(&max))
	exec(t, pool, fmt.Sprintf(`DO $$ BEGIN FOR i IN 1..%d LOOP
		EXECUTE format('CREATE TABLE %s.evict_%%s (x int)', i); END LOOP; END $$`,
		max+50, f.schema))
	for i := 1; i <= max+50; i++ {
		exec(t, pool, fmt.Sprintf("SELECT x FROM %s.evict_%d", f.schema, i))
	}
	a := f.activity(t)
	if !a.Attribution.Dropped || a.Attribution.Source != SourceAudit {
		t.Fatalf("after eviction: %+v, want dropped attribution from the audit",
			a.Attribution)
	}
	if a.Attribution.Reason != ReasonDeallocAdvanced {
		t.Errorf("reason = %q, want dealloc_advanced", a.Attribution.Reason)
	}
	if len(a.Queries) != 2 {
		t.Errorf("fallback audit entries = %d, want 2", len(a.Queries))
	}
}
