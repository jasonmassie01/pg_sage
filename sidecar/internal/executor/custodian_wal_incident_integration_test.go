package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/runway"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Avoided-incident credit for a WAL bound (LEDGER "Avoided-incident
// credit", disk_full_slot): a verified bound earns a near miss only when
// the sampled fill trend projected the declared disk full inside the
// horizon before the action, and the bounded projection after it does
// not. The bound is a real ALTER SYSTEM on the test server, reset after.

func resetSlotKeep(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "ALTER SYSTEM RESET max_slot_wal_keep_size")
		_, _ = pool.Exec(ctx, "SELECT pg_reload_conf()")
	})
}

// seedFillTrend writes 12 samples over 55 minutes of disk usage growing
// at rate bytes/s and flat database size.
func seedFillTrend(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rate float64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.runway_samples WHERE epoch = 'seed'`); err != nil {
		t.Fatalf("clear seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value)
		SELECT 'disk_used', 'cluster', 'seed', now() - make_interval(mins => 55 - i * 5),
		       5e9 + i * 300 * $1::float8
		FROM generate_series(0, 11) i
		UNION ALL
		SELECT 'database_bytes', 'cluster', 'seed',
		       now() - make_interval(mins => 55 - i * 5), 4e9
		FROM generate_series(0, 11) i`, rate); err != nil {
		t.Fatalf("seed trend: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.runway_samples WHERE epoch = 'seed'`)
	})
}

func usedNow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) float64 {
	t.Helper()
	m, err := runway.MeasureDisk(ctx, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), time.Hour)
	if err != nil {
		t.Fatalf("measure disk: %v", err)
	}
	return m.UsedBytes
}

// runWALBound submits one custodian WAL bound of size through the real
// Apply pipeline and returns its action. Each call site uses its own
// size: a failed proposal backs off by its SQL text, so a shared text
// would let one failure cascade into the next test.
func runWALBound(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	cfg *config.Config, size string) int64 {
	t.Helper()
	decisionID := recordCustodianDecision(t, ctx, pool, "wal", "slot_bound")
	exec := New(pool, cfg, zeroTime(), func(string, string, ...any) {})
	exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe, DecisionID: decisionID,
	}})
	if err := exec.SubmitCustodianProposal(ctx, CustodianProposal{Feature: "wal",
		SQL:           "ALTER SYSTEM SET max_slot_wal_keep_size = '" + size + "'",
		TargetObjects: []string{"slot:bench_none"}}); err != nil {
		t.Fatalf("SubmitCustodianProposal: %v", err)
	}
	var actionID int64
	var outcome string
	if err := pool.QueryRow(ctx, `SELECT id, outcome FROM sage.action_log
		WHERE decision_id = $1`, decisionID).Scan(&actionID, &outcome); err != nil {
		t.Fatalf("read action: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("WAL bound outcome = %q, want success", outcome)
	}
	return actionID
}

func TestCustodianWALBoundCreditsAMeasuredDiskNearMiss(t *testing.T) {
	pool, ctx := requireDB(t)
	resetSlotKeep(t, pool)
	// About 2 GB of headroom filling at 1 GB per 10 hours: full in ~20 h,
	// inside the 72 h disk horizon. The 512 MB bound leaves 1.5 GB spare.
	seedFillTrend(t, ctx, pool, 1e9/36000)
	cfg := config.DefaultConfig()
	cfg.Forecaster.DiskCapacityBytes = int64(usedNow(t, ctx, pool) + 2e9)
	actionID := runWALBound(t, ctx, pool, cfg, "512MB")

	var kind, severity, evidence string
	var minutes float64
	if err := pool.QueryRow(ctx, `SELECT kind, severity, credited_minutes::float8,
		evidence_id FROM sage.incident_avoided WHERE action_log_id = $1`, actionID).
		Scan(&kind, &severity, &minutes, &evidence); err != nil {
		t.Fatalf("read incident for action %d: %v", actionID, err)
	}
	if kind != "disk_full_slot" || severity != "near_miss" || minutes != 120 ||
		!strings.HasPrefix(evidence, "incident:disk_full_slot:action:") {
		t.Fatalf("incident = %s/%s/%v/%s", kind, severity, minutes, evidence)
	}
}

// Nothing measured, nothing credited: no declared capacity, a bounded
// projection that still fills the disk, or no sampled trend.
func TestCustodianWALBoundWithoutAMeasuredNearMissEarnsNothing(t *testing.T) {
	pool, ctx := requireDB(t)
	resetSlotKeep(t, pool)
	cases := map[string]struct {
		size  string
		setup func() *config.Config
	}{
		"undeclared capacity": {"576MB", func() *config.Config {
			seedFillTrend(t, ctx, pool, 1e9/36000)
			return config.DefaultConfig()
		}},
		"bound still fills the disk": {"608MB", func() *config.Config {
			seedFillTrend(t, ctx, pool, 1e9/36000)
			cfg := config.DefaultConfig()
			cfg.Forecaster.DiskCapacityBytes = int64(usedNow(t, ctx, pool) + 2e8)
			return cfg
		}},
		"no sampled trend": {"640MB", func() *config.Config {
			_, _ = pool.Exec(ctx, `DELETE FROM sage.runway_samples WHERE epoch = 'seed'`)
			cfg := config.DefaultConfig()
			cfg.Forecaster.DiskCapacityBytes = int64(usedNow(t, ctx, pool) + 2e9)
			return cfg
		}},
	}
	for name, c := range cases {
		actionID := runWALBound(t, ctx, pool, c.setup(), c.size)
		if n := incidentsForAction(t, ctx, pool, actionID); n != 0 {
			t.Fatalf("%s: incidents = %d, want 0", name, n)
		}
	}
}
