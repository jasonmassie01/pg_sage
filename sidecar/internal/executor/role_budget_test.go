package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// market-08: the set_role_timeout and set_role_connection_limit
// contracts without a database: parsing, classification, validation, the
// approval gate and error mapping. role_budget_db_test runs them live.
//
// No concurrent access tests here: parsing and classification are pure;
// the cluster lock's serialization is covered in role_budget_db_test.

const (
	roleTimeoutSQL = `ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '6000ms'`
	roleResetSQL   = `ALTER ROLE "app" IN DATABASE "shop" RESET statement_timeout`
	roleConnSQL    = `ALTER ROLE "app" CONNECTION LIMIT 40`
)

func TestParseRoleBudgetSQL(t *testing.T) {
	got, ok := parseRoleBudgetSQL(roleTimeoutSQL)
	if !ok || got.Role != "app" || got.Database != "shop" || got.Name != "statement_timeout" ||
		got.Value != "6000ms" || got.Reset || got.ConnLimit {
		t.Fatalf("timeout = %+v (%v)", got, ok)
	}
	got, ok = parseRoleBudgetSQL(`alter role Batch in database "My DB" set ` +
		`idle_in_transaction_session_timeout to '2min'`)
	if !ok || got.Role != "batch" || got.Database != "My DB" || got.Value != "2min" {
		t.Fatalf("lower case, TO and quoted database = %+v (%v)", got, ok)
	}
	got, ok = parseRoleBudgetSQL(roleResetSQL)
	if !ok || !got.Reset || got.Value != "" {
		t.Fatalf("reset = %+v (%v)", got, ok)
	}
	got, ok = parseRoleBudgetSQL(`ALTER ROLE "we""ird" CONNECTION LIMIT -1`)
	if !ok || !got.ConnLimit || got.Role != `we"ird` || got.Limit != -1 {
		t.Fatalf("connection limit = %+v (%v)", got, ok)
	}
}

// Invalid input: anything but the two exact forms is no role budget
// statement, and the executor validator refuses it.
func TestRoleBudgetSQL_RefusesOtherForms(t *testing.T) {
	for _, sql := range []string{
		`ALTER ROLE "app" SET statement_timeout = '6s'`, // every database
		`ALTER ROLE "app" IN DATABASE "shop" SET work_mem = '64MB'`,
		`ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '6s; DROP TABLE t'`,
		`ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '-6s'`,
		`ALTER ROLE "app" SUPERUSER`,
		`ALTER ROLE "app" CONNECTION LIMIT 5 SUPERUSER`,
		`ALTER ROLE "app" PASSWORD 'x'`,
		`ALTER ROLE ALL IN DATABASE "shop" SET statement_timeout = '6s'`,
		`ALTER ROLE "app" RENAME TO admin`,
		`ALTER ROLE "app" CONNECTION LIMIT 5; DROP ROLE "app"`,
	} {
		if _, ok := parseRoleBudgetSQL(sql); ok {
			t.Errorf("parsed %q as a role budget statement", sql)
		}
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Errorf("validator accepted %q", sql)
		}
	}
	for _, sql := range []string{roleTimeoutSQL, roleResetSQL, roleConnSQL} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("validator refused %q: %v", sql, err)
		}
	}
}

func TestRoleBudgetSQL_Classification(t *testing.T) {
	for sql, want := range map[string]string{
		roleTimeoutSQL: ActionTypeSetRoleTimeout, roleResetSQL: ActionTypeSetRoleTimeout,
		roleConnSQL:                              ActionTypeSetRoleConnectionLimit,
		`ALTER ROLE "app" SET work_mem = '64MB'`: "",
	} {
		if got := ActionTypeForSQL(sql); got != want {
			t.Errorf("ActionTypeForSQL(%q) = %q, want %q", sql, got, want)
		}
	}
	timeout, ok := ContractForActionType(ActionTypeSetRoleTimeout)
	if !ok || timeout.BaseRiskTier != "moderate" || timeout.RollbackClass != "reversible" ||
		timeout.Validate() != nil {
		t.Fatalf("set_role_timeout contract = %+v", timeout)
	}
	conn, ok := ContractForActionType(ActionTypeSetRoleConnectionLimit)
	if !ok || conn.BaseRiskTier != "high" || conn.Validate() != nil {
		t.Fatalf("set_role_connection_limit contract = %+v", conn)
	}
	if changeClassForActionType(ActionTypeSetRoleTimeout) != string(policy.ChangeConfigGUC) ||
		changeClassForActionType(ActionTypeSetRoleConnectionLimit) !=
			string(policy.ChangeConfigGUC) {
		t.Fatal("role budget actions are not configuration changes")
	}
}

// gateAt is a gate whose runtime grants trust level and every tier.
func gateAt(level string) policy.Gate {
	now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	cfg := usageTestConfig(now, &usageByTarget{}, new(int))
	cfg.Runtime = func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
		return policy.RuntimeState{ExecutorEnabled: true, TrustLevel: level,
			ExecutionMode: policy.ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
			RampStart: now.Add(-365 * 24 * time.Hour), InConfiguredWindow: true}, nil
	}
	return policy.NewGate(cfg)
}

// CHECK-13: the executor refuses set_role_connection_limit without
// approval at every trust level: the gate never executes it unattended.
func TestRoleConnectionLimit_NeverUnattendedAtAnyTrustLevel(t *testing.T) {
	f := analyzer.Finding{Category: "workload_budget_enforce", ObjectType: "role",
		ObjectIdentifier: "app/connection_limit", RecommendedSQL: roleConnSQL,
		RollbackSQL: `ALTER ROLE "app" CONNECTION LIMIT -1`, ActionRisk: "high_risk"}
	for _, level := range []string{policy.TrustObservation, policy.TrustAdvisory,
		policy.TrustAutonomous, "superuser"} {
		d := gateAt(level).Authorize(context.Background(), findingRequest(f, false))
		if d.Verdict == policy.VerdictExecute {
			t.Errorf("trust %s: connection limit executed unattended (%+v)", level, d)
		}
	}
	req := findingRequest(f, false)
	if req.Contract == nil || !slicesContainGuardrail(req.Contract.Guardrails) {
		t.Fatalf("request contract = %+v, want the approval guardrail", req.Contract)
	}
}

func slicesContainGuardrail(gs []policy.Guardrail) bool {
	for _, g := range gs {
		if g == policy.GuardrailApprovalRequired {
			return true
		}
	}
	return false
}

// Error propagation: a permission error from a managed provider is a
// refusal carrying the provider's message; other errors pass through.
func TestRoleBudgetExecError(t *testing.T) {
	perm := &pgconn.PgError{Code: "42501",
		Message: "permission denied to alter role", Detail: "Only roles with the CREATEROLE " +
			"attribute may alter roles"}
	err := roleBudgetExecError(roleConnSQL, perm)
	if !errors.Is(err, ErrRoleBudgetRefused) ||
		!strings.Contains(err.Error(), "permission denied to alter role") ||
		!strings.Contains(err.Error(), "CREATEROLE") {
		t.Fatalf("permission error = %v, want a refusal with the provider message", err)
	}
	other := errors.New("connection reset")
	if err := roleBudgetExecError(roleConnSQL, other); errors.Is(err, ErrRoleBudgetRefused) ||
		!errors.Is(err, other) {
		t.Fatalf("other error = %v", err)
	}
	if roleBudgetExecError(roleConnSQL, nil) != nil {
		t.Fatal("nil error was wrapped")
	}
}

// Rollback statements are themselves role budget statements the
// executor accepts, so a rollback never fails validation; a prior value
// the executor could not set back (a unit it does not accept) has no
// faithful rollback and refuses the change.
func TestRoleBudgetRollback(t *testing.T) {
	stmt := roleBudgetStmt{Role: "app", Database: "shop", Name: "statement_timeout"}
	prior := "30s"
	set, err := roleSettingRollback(stmt, &prior)
	if err != nil || set != `ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '30s'` {
		t.Fatalf("rollback with prior = %q, %v", set, err)
	}
	reset, err := roleSettingRollback(stmt, nil)
	if err != nil || reset != roleResetSQL {
		t.Fatalf("rollback without prior = %q, %v", reset, err)
	}
	for _, sql := range []string{set, reset} {
		if ValidateExecutorSQL(sql) != nil {
			t.Errorf("rollback %q does not validate", sql)
		}
	}
	odd := "1d"
	if got, err := roleSettingRollback(stmt, &odd); !errors.Is(err, ErrNoFaithfulRollback) {
		t.Fatalf("prior 1d: %q, %v, want no faithful rollback", got, err)
	}
}

// State: a connection limit is cluster-wide, so only the fleet leader
// applies it; a timeout (per database) runs on any process.
func TestRoleBudget_ConnectionLimitNeedsTheLeader(t *testing.T) {
	e := &Executor{}
	facts := roleFacts{exists: true, self: "sage", database: "shop", sessions: 3}
	conn, _ := parseRoleBudgetSQL(roleConnSQL)
	timeout, _ := parseRoleBudgetSQL(roleTimeoutSQL)
	if err := e.checkRoleBudget(conn, facts); err != nil {
		t.Fatalf("single process (no leader check): %v", err)
	}
	e.WithLeaderCheck(func() bool { return false })
	if err := e.checkRoleBudget(conn, facts); !errors.Is(err, ErrRoleBudgetRefused) ||
		!strings.Contains(err.Error(), "leader") {
		t.Fatalf("follower: %v, want refused off the leader", err)
	}
	if err := e.checkRoleBudget(timeout, facts); err != nil {
		t.Fatalf("follower timeout: %v", err)
	}
	e.WithLeaderCheck(func() bool { return true })
	if err := e.checkRoleBudget(conn, facts); err != nil {
		t.Fatalf("leader: %v", err)
	}
}
