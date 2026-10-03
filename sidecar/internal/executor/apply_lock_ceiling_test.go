package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// The policy lock ceiling (D1) is applied by Apply to every statement an
// intent runs. D1 wired it into the cycle's DDL and operator DDL only;
// these cover the paths it left out: ANALYZE (cycle and operator) and
// custodian changes. Each case runs against a table held in SHARE UPDATE
// EXCLUSIVE mode (which blocks ANALYZE and reloption changes but not the
// catalog reads around them) with safety.lock_timeout_ms = 4000, and
// compares a 500 ms ceiling with no ceiling, so the ceiling is what
// shortens the wait.

const (
	ceilingSafetyMS = 4000
	ceilingMS       = 500
)

func ceilingExecutor(pool *pgxpool.Pool, ceiling int64) *Executor {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Safety.LockTimeoutMs = ceilingSafetyMS
	e := New(pool, cfg, time.Time{}, nopLog)
	e.emergencyStopFn = func(context.Context) bool { return false }
	e.WithPolicyGate(&reauthGate{lockCeiling: ceiling})
	return e
}

// holdTable creates a probe table and locks it until the test ends.
func holdTable(t *testing.T, pool *pgxpool.Pool, prefix string) string {
	t.Helper()
	table := probeTable(t, pool, prefix)
	lockTable(t, pool, table)
	return table
}

// lockTable holds SHARE UPDATE EXCLUSIVE on table from a connection outside
// the pool, so schema bootstrap on the pool never queues behind it.
func lockTable(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect blocker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "BEGIN; LOCK TABLE public."+table+
		" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock table: %v", err)
	}
}

// assertCeilingBounds requires the capped wait to give up well before the
// safety timeout, and the uncapped control to wait for it. The capped run
// is compared with the control, which pays the same path overhead (a
// loaded runner slows both): a 500 ms ceiling ends the wait 3.5 s sooner,
// so it must finish at least 2 s before the control.
func assertCeilingBounds(t *testing.T, capped, uncapped time.Duration) {
	t.Helper()
	if capped > uncapped-2*time.Second {
		t.Fatalf("with a %dms ceiling the lock wait took %s, the %dms control %s",
			ceilingMS, capped, ceilingSafetyMS, uncapped)
	}
	if uncapped < 3*time.Second {
		t.Fatalf("control without a ceiling took %s, want about %dms",
			uncapped, ceilingSafetyMS)
	}
}

func timeAnalyzeFinding(t *testing.T, pool *pgxpool.Pool, ceiling int64) time.Duration {
	t.Helper()
	table := holdTable(t, pool, "ceiling_analyze")
	e := ceilingExecutor(pool, ceiling)
	f := analyzer.Finding{Category: "stale_statistics",
		ObjectIdentifier: "public." + table, Title: "analyze probe",
		RecommendedSQL: "ANALYZE public." + table}
	intent := e.findingIntent(f, 0, false, nil)
	started := time.Now()
	if _, err := e.Apply(context.Background(), intent); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return time.Since(started)
}

func TestApplyLockCeilingCapsCycleAnalyze(t *testing.T) {
	pool, _ := requireDB(t)
	assertCeilingBounds(t, timeAnalyzeFinding(t, pool, ceilingMS),
		timeAnalyzeFinding(t, pool, 0))
}

func timeCustodian(t *testing.T, pool *pgxpool.Pool, ceiling int64) time.Duration {
	t.Helper()
	table := holdTable(t, pool, "ceiling_custodian")
	e := ceilingExecutor(pool, ceiling)
	started := time.Now()
	err := e.SubmitCustodianProposal(context.Background(), CustodianProposal{
		Feature: "autovacuum_tuning", SQL: fmt.Sprintf(autovacuumProbe, table),
		TargetObjects: []string{"public." + table},
	})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "lock not available") {
		t.Fatalf("custodian on a held table = %v, want a lock timeout", err)
	}
	t.Logf("ceiling %dms: %s after %s", ceiling, err, elapsed)
	return elapsed
}

func TestApplyLockCeilingCapsCustodian(t *testing.T) {
	pool, _ := requireDB(t)
	assertCeilingBounds(t, timeCustodian(t, pool, ceilingMS), timeCustodian(t, pool, 0))
}

func timeOperatorAnalyze(t *testing.T, ceiling int64) time.Duration {
	t.Helper()
	pool, table, findingID := manualFixture(t, "ANALYZE public.{table}")
	lockTable(t, pool, table)
	e := ceilingExecutor(pool, ceiling)
	started := time.Now()
	if _, err := e.ExecuteManual(context.Background(), findingID,
		"ANALYZE public."+table, "", nil); err == nil {
		t.Fatal("operator ANALYZE on a held table succeeded")
	}
	return time.Since(started)
}

// Each run takes its own fixture in a subtest: requireDB holds a
// cross-package lock for the test that called it.
func TestApplyLockCeilingCapsOperatorAnalyze(t *testing.T) {
	var capped, uncapped time.Duration
	var skipped bool
	measure := func(d *time.Duration, ceiling int64) func(*testing.T) {
		return func(t *testing.T) {
			defer func() { skipped = skipped || t.Skipped() }()
			*d = timeOperatorAnalyze(t, ceiling)
		}
	}
	t.Run("ceiling", measure(&capped, ceilingMS))
	t.Run("control", measure(&uncapped, 0))
	if skipped { // the subtest's reason is logged; there is nothing to compare
		t.Skip("a measurement was skipped")
	}
	assertCeilingBounds(t, capped, uncapped)
}
