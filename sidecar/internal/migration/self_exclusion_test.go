package migration

import (
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// The DDL risk score counts the application's queries and lock waiters on
// the table (perf v1.8.3, perf-selfexcl): pg_sage's own sessions (its
// probes, EXPLAINs and plan captures run application query text) are not
// load a migration competes with.

func selfloadDSN() string {
	if dsn := os.Getenv("SAGE_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return os.Getenv("SAGE_TEST_DATABASE_URL")
}

func TestRiskActiveQueriesCountApplicationSessionsOnly(t *testing.T) {
	pool, ctx := requireDB(t)
	w, _, _ := selfload.Start(t, selfloadDSN())
	ra := NewRiskAssessor(pool, func(string, string, ...any) {})
	risk := &DDLRisk{TableName: strings.TrimPrefix(w.AppTable, "public.")}
	ra.fetchActiveQueries(ctx, risk)
	if risk.ActiveQueries != 1 {
		t.Fatalf("active queries on %s = %d, want 1 (the application waiter; the "+
			"pg_sage waiter left out)", w.AppTable, risk.ActiveQueries)
	}
}

func TestRiskPendingLocksCountApplicationWaitersOnly(t *testing.T) {
	pool, ctx := requireDB(t)
	w, _, _ := selfload.Start(t, selfloadDSN())
	ra := NewRiskAssessor(pool, func(string, string, ...any) {})
	risk := &DDLRisk{TableName: strings.TrimPrefix(w.AppTable, "public."),
		SchemaName: "public"}
	ra.fetchPendingLocks(ctx, risk)
	if risk.PendingLocks != 1 {
		t.Fatalf("pending locks on %s = %d, want 1 (the application waiter; the "+
			"pg_sage waiter left out)", w.AppTable, risk.PendingLocks)
	}
}
