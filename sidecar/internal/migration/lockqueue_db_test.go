package migration

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/lockqueue"
)

// migration-05 (CHECK-23): session A holds ACCESS SHARE on t, B waits in
// ALTER TABLE t ADD COLUMN, three readers queue behind B. After one
// nudged lock-chain observation the migration finding is critical with
// waiters = 3 and cancel guidance for B, and the lock-chain finding no
// longer recommends cancelling A.
func TestLockQueue_PileUpBehindWaitingDDL(t *testing.T) {
	pool, ctx := requireDB(t)
	dsn := testdb.SkipUnlessLive(t)
	s := lockqueue.Start(t, dsn, "public.lq_orders", 3)
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	obs := analyzer.NewLockChainObserver(time.Minute)
	var nudges atomic.Int32
	obs.SetNudger(func() { nudges.Add(1) })
	mcfg := &config.MigrationConfig{Enabled: true, ActivityPolling: true}
	advisor := NewAdvisor(pool, mcfg, version, "lqdb", func(string, string, ...any) {}, nil)
	sink := &captureSink{}
	d := NewDetector(pool, advisor, mcfg, func(string, string, ...any) {}).
		WithFindingSink(sink).WithLockQueue(obs)
	obs.SetQueuedDDL(d)

	if _, err := d.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if nudges.Load() != 1 {
		t.Fatalf("waiting DDL nudged the fast path %d times, want 1", nudges.Load())
	}
	acfg := &config.Config{}
	acfg.Analyzer.LockChain = config.LockChainConfig{Enabled: true,
		MinBlockedThreshold: 1, CriticalBlockedThreshold: 10}
	chainFindings, err := analyzer.ProbeLockChains(ctx, pool, acfg, obs)
	if err != nil {
		t.Fatalf("nudged observation: %v", err)
	}
	if _, err := d.PollOnce(ctx); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	assertQueueFinding(t, sink.last(t), s)
	assertNoRootCancel(t, chainFindings, s)
}

func assertQueueFinding(t *testing.T, f MigrationSafetyFinding, s *lockqueue.Scenario) {
	t.Helper()
	if f.Severity != "critical" || f.Detail["lock_queue_waiters"] != 3 ||
		f.Detail["pid"] != s.DDL || f.Detail["lock_queue_root_pid"] != s.Root {
		t.Fatalf("migration finding = severity %q detail %v; want critical, 3 waiters, "+
			"ddl %d, root %d", f.Severity, f.Detail, s.DDL, s.Root)
	}
	guidance := fmt.Sprintf("SELECT pg_cancel_backend(%d)", s.DDL)
	if f.Detail["cancel_guidance_sql"] != guidance ||
		!strings.Contains(f.Recommendation, guidance) {
		t.Fatalf("cancel guidance = %v / %q, want %q", f.Detail["cancel_guidance_sql"],
			f.Recommendation, guidance)
	}
}

func assertNoRootCancel(t *testing.T, findings []analyzer.Finding, s *lockqueue.Scenario) {
	t.Helper()
	rootCancel := fmt.Sprintf("pg_cancel_backend(%d)", s.Root)
	seenRoot := false
	for _, cf := range findings {
		if strings.Contains(cf.RecommendedSQL, rootCancel) {
			t.Fatalf("lock-chain finding still cancels root reader: %q", cf.RecommendedSQL)
		}
		if cf.Detail["pid"] == s.Root {
			seenRoot = true
			if cf.Detail["queued_ddl_pid"] != s.DDL {
				t.Fatalf("root finding does not point to DDL %d: %v", s.DDL, cf.Detail)
			}
		}
	}
	if !seenRoot {
		t.Fatalf("no lock-chain finding for root %d: %+v", s.Root, findings)
	}
}
