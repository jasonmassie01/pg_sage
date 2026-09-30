package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// agedTable creates a table whose relfrozenxid is age transactions old.
// The catalog is edited directly (superuser): the tuples are recent, so a
// real VACUUM (FREEZE) brings the horizon back, as it would in production.
func agedTable(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, age int64,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS public."+table); err != nil {
		t.Fatalf("drop aged table: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+table+
		" (id bigint) WITH (autovacuum_enabled = false)"); err != nil {
		t.Fatalf("create aged table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	if _, err := pool.Exec(ctx, "INSERT INTO public."+table+" VALUES (1)"); err != nil {
		t.Fatalf("seed aged table: %v", err)
	}
	setFrozenXIDAge(t, ctx, pool, table, age)
}

func setFrozenXIDAge(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, age int64,
) {
	t.Helper()
	if age <= 0 {
		return
	}
	tag, err := pool.Exec(ctx, `UPDATE pg_class SET relfrozenxid =
		((pg_snapshot_xmax(pg_current_snapshot())::text::bigint - $2 + 4294967296)
		 % 4294967296)::text::xid WHERE oid = to_regclass($1)`, "public."+table, age)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age relfrozenxid (the test role must be a superuser): rows=%d err=%v",
			tag.RowsAffected(), err)
	}
}

type freezeRun struct {
	actionID, decisionID, verificationID int64
	outcome, verdict                     string
}

// runFreeze submits one custodian freeze through the real Apply pipeline.
func runFreeze(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, table string,
) freezeRun {
	t.Helper()
	decisionID := recordCustodianDecision(t, ctx, pool, "freeze", table)
	exec := New(pool, cfg, zeroTime(), func(string, string, ...any) {})
	exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe, DecisionID: decisionID,
	}})
	if err := exec.SubmitCustodianProposal(ctx, CustodianProposal{
		Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."` + table + `"`,
		TargetObjects: []string{"public." + table},
	}); err != nil {
		t.Fatalf("SubmitCustodianProposal: %v", err)
	}
	run := freezeRun{decisionID: decisionID}
	if err := pool.QueryRow(ctx, `SELECT al.id, al.outcome, COALESCE(v.id, 0),
		COALESCE(v.verdict, '') FROM sage.action_log al
		LEFT JOIN sage.verification v ON v.action_log_id = al.id
		WHERE al.decision_id = $1`, decisionID).Scan(&run.actionID, &run.outcome,
		&run.verificationID, &run.verdict); err != nil {
		t.Fatalf("read freeze action: %v", err)
	}
	if run.outcome != "success" || run.verdict != "success" {
		t.Fatalf("freeze outcome/verdict = %q/%q, want success/success",
			run.outcome, run.verdict)
	}
	return run
}

func incidentsForAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, actionID int64,
) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.incident_avoided
		WHERE action_log_id = $1`, actionID).Scan(&count); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	return count
}

// CHECK: a table inside the red buffer that a freeze returns to green
// earns one conservative near-miss credit tied to the action's decision
// and verification.
func TestCustodianFreezeCreditsNearMissWhenRedTableReturnsToGreen(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "incident_red_probe"
	agedTable(t, ctx, pool, table, 160_000_000)

	run := runFreeze(t, ctx, pool, config.DefaultConfig(), table)

	var kind, severity, evidence string
	var minutes float64
	var decisionID, verificationID int64
	err := pool.QueryRow(ctx, `SELECT kind, severity, credited_minutes::float8,
		evidence_id, decision_id, verification_id FROM sage.incident_avoided
		WHERE action_log_id = $1`, run.actionID).Scan(&kind, &severity, &minutes,
		&evidence, &decisionID, &verificationID)
	if err != nil {
		t.Fatalf("read incident for action %d: %v", run.actionID, err)
	}
	if kind != "xid_wraparound" || severity != "near_miss" || minutes != 120 {
		t.Fatalf("incident = %s/%s/%v, want xid_wraparound/near_miss/120",
			kind, severity, minutes)
	}
	if decisionID != run.decisionID || verificationID != run.verificationID ||
		run.verificationID == 0 {
		t.Fatalf("incident links decision=%d verification=%d, want %d/%d",
			decisionID, verificationID, run.decisionID, run.verificationID)
	}
	if !strings.HasPrefix(evidence, "incident:xid_wraparound:action:") {
		t.Fatalf("evidence id = %q", evidence)
	}
	var age int64
	if err := pool.QueryRow(ctx, `SELECT age(relfrozenxid)::bigint FROM pg_class
		WHERE oid = to_regclass($1)`, "public."+table).Scan(&age); err != nil ||
		age > 1_000_000 {
		t.Fatalf("age after freeze = %d (%v), want far below the 160M baseline", age, err)
	}
}

// The green table is old but outside the amber buffer (50M of 200M). A
// brand-new table is not a valid case: its horizon cannot improve, and
// concurrent transactions make its age grow during the freeze.
func TestCustodianFreezeFromAmberOrGreenEarnsNoIncidentCredit(t *testing.T) {
	pool, ctx := requireDB(t)
	for name, age := range map[string]int64{
		"incident_amber_probe": 120_000_000, "incident_green_probe": 50_000_000,
	} {
		agedTable(t, ctx, pool, name, age)
		run := runFreeze(t, ctx, pool, config.DefaultConfig(), name)
		if count := incidentsForAction(t, ctx, pool, run.actionID); count != 0 {
			t.Fatalf("%s: incidents = %d, want 0 (routine maintenance is toil)", name, count)
		}
	}
}

// The red buffer is configuration: under a 10% buffer a table 160M into a
// 200M horizon is amber, so the same freeze earns nothing.
func TestCustodianFreezeIncidentHonorsConfiguredRedBuffer(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "incident_buffer_probe"
	agedTable(t, ctx, pool, table, 160_000_000)
	cfg := config.DefaultConfig()
	cfg.Custodian.Freeze.RedBufferPct = 10

	run := runFreeze(t, ctx, pool, cfg, table)
	if count := incidentsForAction(t, ctx, pool, run.actionID); count != 0 {
		t.Fatalf("incidents = %d, want 0 under a 10%% red buffer", count)
	}
}

// State transition: each crossing into red is credited once. A second
// freeze while the table is green (50M of 200M) earns nothing; a new
// crossing earns a new credit on its own action.
func TestCustodianFreezeCreditsEachRedCrossingOnce(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "incident_repeat_probe"
	agedTable(t, ctx, pool, table, 170_000_000)
	cfg := config.DefaultConfig()

	first := runFreeze(t, ctx, pool, cfg, table)
	setFrozenXIDAge(t, ctx, pool, table, 50_000_000)
	second := runFreeze(t, ctx, pool, cfg, table)
	setFrozenXIDAge(t, ctx, pool, table, 170_000_000)
	third := runFreeze(t, ctx, pool, cfg, table)

	got := []int{incidentsForAction(t, ctx, pool, first.actionID),
		incidentsForAction(t, ctx, pool, second.actionID),
		incidentsForAction(t, ctx, pool, third.actionID)}
	if got[0] != 1 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("incidents per freeze = %v, want [1 0 1]", got)
	}
}

func TestReadFreezeHorizonMeasuresTheTable(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "incident_horizon_probe"
	agedTable(t, ctx, pool, table, 120_000_000)
	exec := New(pool, config.DefaultConfig(), zeroTime(), func(string, string, ...any) {})

	horizon, err := exec.readFreezeHorizon(ctx, "public."+table)
	if err != nil {
		t.Fatalf("readFreezeHorizon: %v", err)
	}
	var xidMax, mxidMax int64
	if err := pool.QueryRow(ctx, `SELECT current_setting('autovacuum_freeze_max_age')::bigint,
		current_setting('autovacuum_multixact_freeze_max_age')::bigint`).
		Scan(&xidMax, &mxidMax); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if horizon.xidAge < 120_000_000 || horizon.xidAge > 120_100_000 ||
		horizon.xidMax != xidMax || horizon.mxidMax != mxidMax || horizon.mxidAge < 0 {
		t.Fatalf("horizon = %+v, want xid age ~120M and max %d/%d", horizon, xidMax, mxidMax)
	}

	_, err = exec.readFreezeHorizon(ctx, "public.incident_no_such_table")
	if err == nil || !strings.Contains(err.Error(), "freeze horizon") {
		t.Fatalf("missing table error = %v, want a freeze horizon error", err)
	}
}

// Error propagation: credit is bookkeeping after a successful action. When
// the after-measurement cannot be taken the credit is withheld, the reason
// is logged with the action id, and nothing is written.
func TestCustodianFreezeIncidentWithheldWhenHorizonUnreadable(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "incident_unreadable_probe"
	agedTable(t, ctx, pool, table, 160_000_000)
	var logged []string
	exec := New(pool, config.DefaultConfig(), zeroTime(),
		func(component, format string, args ...any) {
			logged = append(logged, component+": "+fmt.Sprintf(format, args...))
		})
	before, err := exec.readFreezeHorizon(ctx, "public."+table)
	if err != nil {
		t.Fatalf("readFreezeHorizon: %v", err)
	}
	run := &custodianRun{executor: exec, baseline: custodianBaseline{horizon: &before},
		proposal: CustodianProposal{Feature: "freeze",
			TargetObjects: []string{"public." + table}}}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	const actionID = int64(1 << 40)
	run.creditFreezeIncident(cancelled, actionID)

	if len(logged) != 1 || !strings.Contains(logged[0], "incident credit for action") ||
		!strings.Contains(logged[0], "withheld") ||
		!strings.Contains(logged[0], "freeze horizon") {
		t.Fatalf("log = %q, want one withheld-credit line naming the horizon read", logged)
	}
	if count := incidentsForAction(t, ctx, pool, actionID); count != 0 {
		t.Fatalf("incidents = %d, want 0", count)
	}
}

// A red table whose action is not a verified success earns nothing: the
// ledger refuses the credit and the refusal is logged.
func TestCustodianFreezeIncidentRefusedForUnverifiedAction(t *testing.T) {
	pool, ctx := requireDB(t)
	const table = "incident_unverified_probe"
	agedTable(t, ctx, pool, table, 0)
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('vacuum', 'VACUUM t', 'monitoring')
		RETURNING id`).Scan(&actionID); err != nil {
		t.Fatalf("insert unverified action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", actionID)
	})
	var logged []string
	exec := New(pool, config.DefaultConfig(), zeroTime(),
		func(component, format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		})
	red := freezeHorizon{xidAge: 160_000_000, xidMax: 200_000_000, mxidMax: 400_000_000}
	run := &custodianRun{executor: exec, baseline: custodianBaseline{horizon: &red},
		proposal: CustodianProposal{Feature: "freeze",
			TargetObjects: []string{"public." + table}}}

	run.creditFreezeIncident(ctx, actionID)

	if count := incidentsForAction(t, ctx, pool, actionID); count != 0 {
		t.Fatalf("incidents = %d, want 0 for an unverified action", count)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "not eligible") {
		t.Fatalf("log = %q, want one not-eligible line", logged)
	}
}
