package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// reauthGate authorizes every call before refuseAt (1-based; 0 never
// refuses) and refuses that call and every later one. It records how long each call had left
// before its context deadline (zero: no deadline).
type reauthGate struct {
	mu          sync.Mutex
	refuseAt    int
	decisionID  int64
	lockCeiling int64
	remaining   []time.Duration
}

func (g *reauthGate) Authorize(ctx context.Context, _ policy.ActionRequest) policy.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	var left time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	g.remaining = append(g.remaining, left)
	if g.refuseAt > 0 && len(g.remaining) >= g.refuseAt {
		return policy.Decision{Verdict: policy.VerdictBlocked, RiskTier: policy.RiskSafe,
			Reason: policy.ReasonEmergencyStop}
	}
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
		DecisionID: g.decisionID, LockCeilingMS: g.lockCeiling}
}

// Explain previews an operator action as executable, so the operator path
// reaches its authorizations.
func (g *reauthGate) Explain(context.Context, policy.ActionRequest) policy.Decision {
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe}
}

// assertRefusedUnderDeadline requires exactly want authorizations, the last
// being the refused one, made under a bounded execution deadline.
func assertRefusedUnderDeadline(t *testing.T, g *reauthGate, want int, budget time.Duration) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.remaining) != want {
		t.Fatalf("authorizations = %d, want %d (the last is the re-authorization)",
			len(g.remaining), want)
	}
	left := g.remaining[want-1]
	if left <= 0 || left > budget {
		t.Fatalf("re-authorization deadline left %v, want within (0, %v]", left, budget)
	}
}

// applyBudget is the longest execution deadline Apply may set for cfg.
func applyBudget(cfg *config.Config) time.Duration {
	timeout := cfg.Safety.DDLTimeout()
	if timeout <= 0 {
		timeout = time.Duration(config.DefaultDDLTimeoutSeconds) * time.Second
	}
	return timeout + 2*time.Minute
}

const autovacuumProbe = "ALTER TABLE public.%s SET (autovacuum_vacuum_scale_factor = 0.03)"

// probeTable creates a table whose reloptions show whether an action ran.
func probeTable(t *testing.T, pool *pgxpool.Pool, prefix string) string {
	t.Helper()
	table := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := pool.Exec(context.Background(),
		"CREATE TABLE public."+table+" (id bigint)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	return table
}

func assertProbeUntouched(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	var options []string
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(reloptions, '{}')
		FROM pg_class WHERE oid = to_regclass($1)`, "public."+table).Scan(&options); err != nil {
		t.Fatalf("read reloptions: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("refused re-authorization still executed: reloptions=%v", options)
	}
}

func TestApplyEntryRunCycleReauthorizesUnderDeadline(t *testing.T) {
	pool, ctx := requireDB(t)
	table := probeTable(t, pool, "apply_cycle")
	sql := fmt.Sprintf(autovacuumProbe, table)
	finding := analyzer.Finding{
		Category: "apply_probe", ObjectIdentifier: "public." + table,
		Title: "apply probe", RecommendedSQL: sql, ActionRisk: "safe",
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		VALUES ($1, 'warning', 'table', $2, 'apply probe', '{}', 'rec', $3)`,
		finding.Category, finding.ObjectIdentifier, sql); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	a := &analyzer.Analyzer{}
	a.SetFindings([]analyzer.Finding{finding})
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	e := New(pool, cfg, a, time.Now().Add(-90*24*time.Hour), nopLog)
	e.emergencyStopFn = func(context.Context) bool { return false }
	gate := &reauthGate{refuseAt: 3,
		decisionID: recordCustodianDecision(t, ctx, pool, "apply_probe", table)}
	e.WithPolicyGate(gate)

	e.RunCycle(ctx, false)

	assertRefusedUnderDeadline(t, gate, 3, applyBudget(cfg))
	assertProbeUntouched(t, pool, table)
}

func TestApplyEntryCustodianReauthorizesUnderMandatoryDeadline(t *testing.T) {
	pool, ctx := requireDB(t)
	table := probeTable(t, pool, "apply_custodian")
	cfg := config.DefaultConfig()
	// No configured DDL timeout: the execution deadline is still mandatory.
	cfg.Safety.DDLTimeoutSeconds = 0
	e := New(pool, cfg, nil, zeroTime(), nopLog)
	e.emergencyStopFn = func(context.Context) bool { return false }
	gate := &reauthGate{refuseAt: 2,
		decisionID: recordCustodianDecision(t, ctx, pool, "autovacuum_tuning", table)}
	e.WithPolicyGate(gate)

	err := e.SubmitCustodianProposal(ctx, CustodianProposal{
		Feature: "autovacuum_tuning", SQL: fmt.Sprintf(autovacuumProbe, table),
		TargetObjects: []string{"public." + table},
	})

	if !errors.Is(err, ErrCustodianProposalWithheld) {
		t.Fatalf("SubmitCustodianProposal = %v, want withheld", err)
	}
	assertRefusedUnderDeadline(t, gate, 2, applyBudget(cfg))
	assertProbeUntouched(t, pool, table)
}

func TestApplyEntryVerifiedIndexReauthorizesUnderDeadline(t *testing.T) {
	e := manualExecutor(nil)
	gate := &reauthGate{refuseAt: 2}
	e.WithPolicyGate(gate)
	actions := &fakeVerifiedIndexActions{}
	e.indexVerification = newVerifiedIndexLifecycle(
		&fakeIndexVerifier{admission: verify.Admission{OK: true}}, actions)

	err := e.SubmitVerifiedIndexProposal(context.Background(), CustodianProposal{
		Feature: "fk_index", TargetObjects: []string{"public.orders"},
		SQL: "CREATE INDEX CONCURRENTLY orders_fk_idx ON public.orders (customer_id)",
	}, "DROP INDEX CONCURRENTLY IF EXISTS public.orders_fk_idx", []int64{7})

	if !errors.Is(err, ErrCustodianProposalWithheld) {
		t.Fatalf("SubmitVerifiedIndexProposal = %v, want withheld", err)
	}
	assertRefusedUnderDeadline(t, gate, 2, applyBudget(e.cfg))
}

func TestApplyEntryManualReauthorizesUnderDeadline(t *testing.T) {
	pool, table, findingID := manualFixture(t,
		"ALTER TABLE public.{table} SET (autovacuum_vacuum_scale_factor = 0.03)")
	e := manualExecutor(pool)
	gate := &reauthGate{refuseAt: 2}
	e.WithPolicyGate(gate)

	_, err := e.ExecuteManual(context.Background(), findingID,
		fmt.Sprintf(autovacuumProbe, table), "", nil)

	if err == nil || !strings.Contains(err.Error(), "policy refused operator action") {
		t.Fatalf("ExecuteManual = %v, want refused by the re-authorization", err)
	}
	assertRefusedUnderDeadline(t, gate, 2, applyBudget(e.cfg))
	assertProbeUntouched(t, pool, table)
}

// Retention deletes run in the schema guard; the executor entry is its
// authorization, which Apply bounds like every other stage.
func TestApplyEntryRetentionAuthorizesUnderDeadline(t *testing.T) {
	e := manualExecutor(nil)
	gate := &reauthGate{refuseAt: 1}
	e.WithPolicyGate(gate)

	err := e.AuthorizeRetention(context.Background(), RetentionRequest{
		Target: "public.events", Column: "created_at", Cutoff: time.Now(),
		Window: 24 * time.Hour, BatchLimit: 100, Candidates: 5,
	})

	if !errors.Is(err, ErrCustodianProposalWithheld) {
		t.Fatalf("AuthorizeRetention = %v, want withheld", err)
	}
	assertRefusedUnderDeadline(t, gate, 1, applyBudget(e.cfg))
}
