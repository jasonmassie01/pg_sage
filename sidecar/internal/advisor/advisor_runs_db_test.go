//go:build integration

package advisor

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
)

// advisors-02 on real PostgreSQL: the schedule survives a restart, the
// advisor_runs row lock serializes concurrent cycles, and a setting the
// DBA applied by hand resolves its open finding within one cycle.

func runsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.advisor_runs`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings
			WHERE category IN ('wal_tuning', 'connection_tuning')`)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func liveSettings(t *testing.T, pool *pgxpool.Pool) *collector.Snapshot {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT name, setting,
		COALESCE(unit, ''), context, pending_restart FROM pg_settings`)
	if err != nil {
		t.Fatalf("pg_settings: %v", err)
	}
	defer rows.Close()
	cfg := &collector.ConfigSnapshot{}
	for rows.Next() {
		var s collector.PGSetting
		if err := rows.Scan(&s.Name, &s.Setting, &s.Unit, &s.Context,
			&s.PendingRestart); err != nil {
			t.Fatalf("scan pg_settings: %v", err)
		}
		cfg.PGSettings = append(cfg.PGSettings, s)
	}
	return &collector.Snapshot{CollectedAt: time.Now(), ConfigData: cfg}
}

func dbAdvisor(pool *pgxpool.Pool, t *testing.T) (*Advisor, func() []capturedPrompt) {
	mgr, capture := capturingManager(t, "[]")
	cfg := walOnlyConfig("")
	cfg.Advisor.IntervalSeconds = 86400
	return New(pool, cfg, nil, mgr, noopLog), capture
}

// CHECK-18: a restart within the interval reads the persisted run and
// makes no model call.
func TestAdvisorRuns_RestartMakesNoCall_LivePG(t *testing.T) {
	pool := runsPool(t)
	ctx := context.Background()
	first, capture := dbAdvisor(pool, t)
	first.analyzeSnapshot(ctx, liveSettings(t, pool))
	if n := len(capture()); n != 1 {
		t.Fatalf("first run: model calls = %d, want 1", n)
	}
	var last, next time.Time
	var failures int
	if err := pool.QueryRow(ctx, `SELECT last_success_at, consecutive_failures,
		next_attempt_at FROM sage.advisor_runs WHERE sub_advisor = 'wal'
		AND database_name = current_database()`).Scan(&last, &failures, &next); err != nil {
		t.Fatalf("read advisor_runs: %v", err)
	}
	if failures != 0 || next.Sub(last) != 24*time.Hour || time.Since(last) > time.Minute {
		t.Fatalf("row: last=%v failures=%d next=%v, want a fresh success +24h",
			last, failures, next)
	}
	restarted, capture2 := dbAdvisor(pool, t)
	restarted.loadSchedule(ctx)
	if restarted.schedule.get("wal").due(time.Now()) {
		t.Fatal("restart: wal is due after loading the persisted schedule")
	}
	restarted.analyzeSnapshot(ctx, liveSettings(t, pool))
	if n := len(capture2()); n != 0 {
		t.Fatalf("restart within the interval: model calls = %d, want 0", n)
	}
}

// A failed run is persisted as a failure: no success, next try in 1h.
func TestAdvisorRuns_FailurePersisted_LivePG(t *testing.T) {
	pool := runsPool(t)
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()
	cfg := walOnlyConfig("")
	cfg.Advisor.IntervalSeconds = 86400
	a := New(pool, cfg, nil, mgr, noopLog)
	a.analyzeSnapshot(context.Background(), liveSettings(t, pool))
	var last *time.Time
	var failures int
	var backoff float64
	if err := pool.QueryRow(context.Background(), `SELECT last_success_at,
		consecutive_failures, extract(epoch FROM next_attempt_at - updated_at)
		FROM sage.advisor_runs WHERE sub_advisor = 'wal'`).Scan(&last, &failures,
		&backoff); err != nil {
		t.Fatalf("read advisor_runs: %v", err)
	}
	if last != nil || failures != 1 || backoff < 3500 || backoff > 3700 {
		t.Fatalf("row: last=%v failures=%d backoff=%.0fs, want NULL, 1, ~3600s",
			last, failures, backoff)
	}
}

// Concurrency: four cycles on two advisors (two replicas) in parallel make
// one model call; the row lock on sage.advisor_runs serializes the claim.
func TestAdvisorRuns_ParallelCyclesOneCall_LivePG(t *testing.T) {
	pool := runsPool(t)
	mgr, capture := capturingManager(t, "[]")
	cfg := walOnlyConfig("")
	cfg.Advisor.IntervalSeconds = 86400
	advisors := []*Advisor{New(pool, cfg, nil, mgr, noopLog), New(pool, cfg, nil, mgr, noopLog)}
	snap := liveSettings(t, pool)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(a *Advisor) {
			defer wg.Done()
			<-start
			a.analyzeSnapshot(context.Background(), snap)
		}(advisors[i%2])
	}
	close(start)
	wg.Wait()
	if n := len(capture()); n != 1 {
		t.Fatalf("model calls = %d, want 1 for four parallel cycles", n)
	}
}

// CHECK-17: after a manual ALTER SYSTEM matching the finding and a reload,
// the finding resolves within one cycle; an unapplied one stays open.
func TestAdvisorRuns_ManualAlterSystemResolves_LivePG(t *testing.T) {
	pool := runsPool(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER SYSTEM RESET max_wal_size`)
		_, _ = pool.Exec(context.Background(), `SELECT pg_reload_conf()`)
	})
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, recommendation, recommended_sql) VALUES
		('wal_tuning', 'info', 'configuration', 'instance', 'raise max_wal_size', '{}',
		 'r', 'ALTER SYSTEM SET max_wal_size = ''3GB'';'),
		('connection_tuning', 'info', 'configuration', 'instance', 'idle timeout', '{}',
		 'r', 'ALTER SYSTEM SET idle_in_transaction_session_timeout = ''17min'';')`)
	if err != nil {
		t.Fatalf("insert open findings: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER SYSTEM SET max_wal_size = '3072MB'`); err != nil {
		t.Skipf("ALTER SYSTEM needs a superuser test server: %v", err)
	}
	waitForSetting(t, pool, "max_wal_size", "3072")
	a, _ := dbAdvisor(pool, t)
	a.cfg.Advisor.ConnectionEnabled = true
	out := a.analyzeSnapshot(ctx, liveSettings(t, pool))
	for _, cat := range []string{"wal_tuning", "connection_tuning"} {
		if !slices.Contains(a.LastEvaluatedCategories(), cat) {
			t.Fatalf("evaluated = %v, want %s", a.LastEvaluatedCategories(), cat)
		}
		active := map[string]bool{}
		for _, f := range out {
			if f.Category == cat {
				active[f.ObjectIdentifier] = true
			}
		}
		if _, err := analyzer.ResolveClearedFindings(ctx, pool, active, cat); err != nil {
			t.Fatalf("resolve %s: %v", cat, err)
		}
	}
	for cat, want := range map[string]string{"wal_tuning": "resolved",
		"connection_tuning": "open"} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM sage.findings
			WHERE category = $1 ORDER BY id DESC LIMIT 1`, cat).Scan(&status); err != nil {
			t.Fatalf("read %s: %v", cat, err)
		}
		if status != want {
			t.Errorf("%s status = %q, want %q", cat, status, want)
		}
	}
}

func waitForSetting(t *testing.T, pool *pgxpool.Pool, name, want string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_reload_conf()`); err != nil {
		t.Fatalf("reload: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got string
		// A fresh session sees the reloaded value.
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		err = conn.QueryRow(ctx, `SELECT setting FROM pg_settings WHERE name = $1`,
			name).Scan(&got)
		conn.Release()
		if err == nil && got == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never became %s after reload", name, want)
}
