package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

// collection-08 on a live server: what used to reach only the logs (a
// category that failed, a snapshot that could not be stored) is in Status.

func TestStatusUnavailableCategoryAfterCycleDB(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.overrideStep("indexes", func(context.Context, *Snapshot) error {
		return errors.New("injected: permission denied for table pg_index")
	})
	before := time.Now()
	c.cycle(ctx, &fakeTicker{})
	st := c.Status()
	if !strings.Contains(st.Unavailable["indexes"], "permission denied") {
		t.Fatalf("Unavailable = %v, want indexes with the injected error", st.Unavailable)
	}
	if _, ok := st.Unavailable["tables"]; ok {
		t.Errorf("tables marked unavailable without a failure: %v", st.Unavailable)
	}
	if st.BreakerState != BreakerClosed || st.LastSuccess.Before(before) {
		t.Errorf("state/last success = %q/%v, want closed and set by this cycle",
			st.BreakerState, st.LastSuccess)
	}
	if st.PersistFailures != 0 {
		t.Errorf("PersistFailures = %d on a bootstrapped database, want 0 (last error %q)",
			st.PersistFailures, st.LastErrorText)
	}
	if st.LastLoadRatio < 0 || st.LastLoadRatio > st.LoadCeiling {
		t.Errorf("LastLoadRatio = %v, want the measured ratio under the ceiling %v",
			st.LastLoadRatio, st.LoadCeiling)
	}
}

// restrictedPool connects as a new role that can read the catalog and the
// sage schema but cannot INSERT into sage.snapshots.
func restrictedPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("sage_c08_%d", time.Now().UnixNano())
	const password = "c08-test-only"
	stmts := []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD '" + password + "'",
		"GRANT pg_monitor TO " + role,
		"GRANT USAGE ON SCHEMA sage TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA sage TO " + role,
	}
	for _, s := range stmts {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = password
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(func() {
		pool.Close()
		for _, s := range []string{"DROP OWNED BY " + role, "DROP ROLE " + role} {
			if _, err := admin.Exec(ctx, s); err != nil {
				t.Logf("cleanup %s: %v", s, err)
			}
		}
	})
	var canInsert bool
	err = pool.QueryRow(ctx, "SELECT has_table_privilege('sage.snapshots', 'INSERT')").
		Scan(&canInsert)
	if err != nil || canInsert {
		t.Fatalf("restricted role can insert = %v (err %v), want false", canInsert, err)
	}
	return pool
}

func TestStatusCountsPersistFailureDB(t *testing.T) {
	admin := testPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, admin); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	pool := restrictedPool(t, admin)
	rec := &logRecorder{}
	c := New(pool, testConfig(), serverVersion(t, admin), rec.log)
	c.cycle(ctx, &fakeTicker{})
	st := c.Status()
	if c.LatestSnapshot() == nil {
		t.Fatalf("no snapshot collected as the restricted role; logs: %v", rec.lines)
	}
	if st.PersistFailures != 1 {
		t.Fatalf("PersistFailures = %d, want 1; logs: %v", st.PersistFailures, rec.lines)
	}
	if !strings.HasPrefix(st.LastErrorText, "persist") &&
		!strings.HasPrefix(st.LastErrorText, "query_store") {
		t.Errorf("LastErrorText = %q, want a persist or query_store error", st.LastErrorText)
	}
	if !rec.has("ERROR", "permission denied") {
		t.Errorf("persist failure not logged with its cause; logs: %v", rec.lines)
	}
	if st.BreakerState != BreakerClosed || st.LastSuccess.IsZero() {
		t.Errorf("a collected snapshot is a success: state %q, last success %v",
			st.BreakerState, st.LastSuccess)
	}
	c.cycle(ctx, &fakeTicker{})
	if got := c.Status().PersistFailures; got != 2 {
		t.Errorf("PersistFailures = %d after a second cycle, want 2 (monotonic)", got)
	}
}

// The live load query feeds the breaker's Decision: an idle server is
// under a 99% ceiling, and a cancelled context is a load query error, not
// load.
func TestBreakerDecisionOnLiveServerDB(t *testing.T) {
	pool := testPool(t)
	cb := NewCircuitBreaker(99, 5)
	d := cb.ShouldSkip(context.Background(), pool)
	if d.Skip || d.Err != nil || d.Ratio < 0 || d.Ratio > 0.99 {
		t.Errorf("idle server decision = %+v, want no skip and a measured ratio", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d = cb.ShouldSkip(ctx, pool)
	if !d.Skip || d.Reason != SkipReasonLoadQueryError || !errors.Is(d.Err, context.Canceled) {
		t.Errorf("cancelled decision = %+v, want a load_query_error wrapping Canceled", d)
	}
}
