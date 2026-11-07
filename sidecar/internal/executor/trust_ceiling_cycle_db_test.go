package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// config-01: RunCycle's apply path reads the live rollback cooldown. The
// helper-level test cannot see a regression that puts apply_finding back on
// e.cfg; this one drives a whole cycle on real PostgreSQL.

func TestTrustCeilingRaisedCooldownHoldsFindingInRunCycleOnPostgres(t *testing.T) {
	pool, ctx := requireDB(t)
	_ = SetEmergencyStop(ctx, pool, false, "test")
	findingID, title := seedFindingRolledBackTwoDaysAgo(ctx, t, pool)
	cfg := cycleCooldownConfig()
	var mu sync.Mutex
	var logs []string
	logFn := func(_, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(msg, args...))
	}
	e := New(pool, cfg, time.Now().Add(-40*24*time.Hour), logFn)
	// The gate is pinned to execute: the cooldown check runs after it, and
	// the shared test database's ledger and budget must not stop the cycle
	// first. The gate's ceiling refusals have their own tests.
	e.WithEmergencyStopCheck(func(context.Context) bool { return false })
	e.WithPolicyGate(typedTestGate{policy.Decision{Verdict: policy.VerdictExecute}})
	raised := ceilingFromTrust(cfg.Trust)
	raised.RollbackCooldownDays = 7
	e.SetTrustCeiling(&raised)
	e.RunCycle(ctx, false)

	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	want := fmt.Sprintf("skipping %q — rolled back recently (cooldown)", title)
	if !strings.Contains(joined, want) {
		t.Fatalf("cycle logs lack %q: the 1-day startup cooldown was used, not the "+
			"live 7-day one\n%s", want, joined)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_log
		WHERE finding_id = $1 AND outcome <> 'rolled_back'`,
		findingID).Scan(&attempts); err != nil {
		t.Fatalf("count action attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("finding inside the live cooldown was attempted %d times", attempts)
	}
}

// seedFindingRolledBackTwoDaysAgo stores a verified SAFE CREATE INDEX
// finding, proposed durably, whose last action was rolled back 2 days ago.
func seedFindingRolledBackTwoDaysAgo(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool,
) (int64, string) {
	t.Helper()
	suffix := time.Now().UTC().Format("150405000000")
	object := "public.ceiling_cycle_" + suffix
	createSQL := "CREATE INDEX CONCURRENTLY idx_ceiling_cycle_" + suffix +
		" ON " + object + " (id)"
	title := "ceiling cooldown cycle " + suffix
	var findingID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql)
		VALUES ('ceiling_cycle_cat', 'warning', 'index', $1, $2,
		        '{"what_if_verdict": "verified"}', 'rec', $3)
		RETURNING id`, object, title, createSQL).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE finding_id = $1", findingID)
		_, _ = pool.Exec(bg, "DELETE FROM sage.findings WHERE id = $1", findingID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log
		(action_type, finding_id, sql_executed, outcome, executed_at)
		VALUES ('create_index', $1, $2, 'rolled_back', now() - interval '2 days')`,
		findingID, createSQL); err != nil {
		t.Fatalf("insert rolled-back action: %v", err)
	}
	proposeDurable(t, pool, []analyzer.Finding{{
		Category: "ceiling_cycle_cat", Severity: "warning", ObjectIdentifier: object,
		Title: title, Detail: map[string]any{"what_if_verdict": "verified"},
		RecommendedSQL: createSQL, ActionRisk: "safe",
	}})
	return findingID, title
}

// cycleCooldownConfig is autonomous with a 1-day rollback cooldown, so only
// the live ceiling can hold a finding rolled back 2 days ago.
func cycleCooldownConfig() *config.Config {
	return &config.Config{
		Trust: config.TrustConfig{
			Level: "autonomous", Tier3Safe: true, Tier3Moderate: true,
			MaintenanceWindow: "always", CascadeCooldownCycles: 3,
			RollbackCooldownDays: 1,
		},
		Collector: config.CollectorConfig{IntervalSeconds: 60},
		Safety:    config.SafetyConfig{DDLTimeoutSeconds: 30, LockTimeoutMs: 5000},
	}
}
