package tuner

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Hinted EXPLAIN (tuning-05): before a hint is proposed, pg_sage plans the
// statement twice in one bounded READ ONLY transaction, plain and with the
// hint, and compares the plans' shapes (planhash) and root total costs. A
// hint pg_hint_plan ignores (an unknown alias or index) plans the same and is
// refused; a join or scan hint whose estimated cost rises past the tolerance
// is refused. Costs are the planner's estimates: the executor's post-apply
// verification (verify.ClassQueryHint) stays the authority on the outcome.
// Never ANALYZE, never a write, never a LOAD into an application session.

// HintVerdict is the outcome of a hinted EXPLAIN comparison.
type HintVerdict string

const (
	// HintChanged: the hint changes the plan within the cost tolerance, or
	// (basis spill) a Set(work_mem) hint for a plan whose cached ANALYZE
	// output spills. Admitted.
	HintChanged HintVerdict = "changed"
	// HintNoEffect: the hinted plan is the plain plan. Refused.
	HintNoEffect HintVerdict = "no_effect"
	// HintCostRegression: a join or scan hint raises the estimated cost past
	// tuner.hint_cost_tolerance_pct. Refused.
	HintCostRegression HintVerdict = "cost_regression"
	// HintUnverifiable: the comparison cannot run (pg_hint_plan not loaded
	// in pg_sage's session, a parameterized statement before PG16, a
	// Set(work_mem) hint without spill evidence). The finding stays advisory.
	HintUnverifiable HintVerdict = "unverifiable"
	// HintExplainTimeout: an EXPLAIN hit its statement or lock timeout.
	// Refused.
	HintExplainTimeout HintVerdict = "explain_timeout"
	// HintExplainFailed: an EXPLAIN failed otherwise. Refused.
	HintExplainFailed HintVerdict = "explain_failed"
	// HintInvalidStatement: the statement is not one explainable statement.
	// Refused before any EXPLAIN.
	HintInvalidStatement HintVerdict = "invalid_statement"
)

// Refused reports a verdict that refuses the hint.
func (v HintVerdict) Refused() bool {
	switch v {
	case HintChanged, HintUnverifiable:
		return false
	}
	return true
}

// ErrHintRefused: the hinted EXPLAIN refused the hint (no_effect,
// cost_regression, explain_timeout, explain_failed, invalid_statement).
var ErrHintRefused = errors.New("hinted EXPLAIN refused the hint")

// DefaultHintCostTolerancePct is tuner.hint_cost_tolerance_pct unset.
const DefaultHintCostTolerancePct = 10

// genericPlanMajor is the first major with EXPLAIN (GENERIC_PLAN).
const genericPlanMajor = 16

// HintComparison is one hinted EXPLAIN comparison.
type HintComparison struct {
	Verdict HintVerdict
	// Basis of a changed verdict: "plan" (the plan changed) or "spill".
	Basis        string
	Reason       string
	PGMajor      int
	Generic      bool
	TolerancePct float64
	PlainCost    float64
	HintedCost   float64
	PlainHash    string
	HintedHash   string
	PlainPlan    []byte
	HintedPlan   []byte
}

// CompareOptions bound and tune one comparison.
type CompareOptions struct {
	// Timeouts bound the transaction (the tuner's catalog timeouts).
	Timeouts catalogread.Timeouts
	// TolerancePct is the cost rise a join or scan hint may cause (0-100).
	TolerancePct float64
	// SessionLoad: pg_sage may LOAD 'pg_hint_plan' into its own backend
	// when the module is not loaded there (tuning-04's SessionLoad fact).
	SessionLoad bool
	// SpillPlan is the statement's cached EXPLAIN ANALYZE plan; a
	// Set(work_mem) hint that changes no plan shape is admitted only when
	// it shows a disk sort or a multi-batch hash.
	SpillPlan []byte
}

// CompareHint plans stmt plain and with hint and judges the hint. pgMajor
// is the server's major version (0: read it). A refusal returns its verdict
// and an error wrapping ErrHintRefused; unverifiable is not an error.
func CompareHint(ctx context.Context, db catalogread.Beginner, stmt, hint string,
	pgMajor int, opts CompareOptions) (HintComparison, error) {
	cmp := HintComparison{PGMajor: pgMajor, TolerancePct: opts.TolerancePct}
	body, why := explainableBody(stmt, hint)
	if why != "" {
		return refuse(cmp, HintInvalidStatement, why)
	}
	if why := statementUnverifiable(body); why != "" {
		return unverifiable(cmp, why), nil
	}
	parameterized := hasBindParam(body)
	if pgMajor > 0 && parameterized && pgMajor < genericPlanMajor {
		return unverifiable(cmp, paramBeforePG16), nil
	}
	tx, err := catalogread.Begin(ctx, db, opts.Timeouts)
	if err != nil {
		return refuse(cmp, HintExplainFailed, err.Error())
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return compareInTx(ctx, tx, cmp, body, hint, parameterized, opts)
}

const paramBeforePG16 = "a parameterized statement has no plan before PostgreSQL 16 " +
	"(EXPLAIN GENERIC_PLAN)"

// compareInTx runs the comparison's statements in the bounded transaction.
func compareInTx(ctx context.Context, tx pgx.Tx, cmp HintComparison, body, hint string,
	parameterized bool, opts CompareOptions) (HintComparison, error) {
	if cmp.PGMajor <= 0 {
		major, err := serverMajor(ctx, tx)
		if err != nil {
			return refuse(cmp, HintExplainFailed, err.Error())
		}
		cmp.PGMajor = major
		if parameterized && major < genericPlanMajor {
			return unverifiable(cmp, paramBeforePG16), nil
		}
	}
	loaded, err := ensureHintModule(ctx, tx, opts.SessionLoad)
	if err != nil {
		return unverifiable(cmp, err.Error()), nil
	}
	if !loaded {
		return unverifiable(cmp, "pg_hint_plan is not loaded in pg_sage's session "+
			"(not preloaded for its role and LOAD unavailable)"), nil
	}
	if _, err := tx.Exec(ctx, hintSessionSQL); err != nil {
		return refuse(cmp, HintExplainFailed, "configure pg_hint_plan: "+err.Error())
	}
	// Last, so pg_sage's own statements above ran on its own path: the
	// statement is the application's, planned on its path (security-01c-n).
	if err := apppath.Pin(ctx, tx); err != nil {
		return refuse(cmp, HintExplainFailed, err.Error())
	}
	cmp.Generic = parameterized
	explain := "EXPLAIN (FORMAT JSON) "
	if parameterized {
		explain = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	plain, err := explainJSON(ctx, tx, explain+body)
	if err != nil {
		return refuseExplain(cmp, "plain", err)
	}
	hinted, err := explainJSON(ctx, tx, "/*+ "+hint+" */ "+explain+body)
	if err != nil {
		return refuseExplain(cmp, "hinted", err)
	}
	return judged(cmp, plain, hinted, hint, opts)
}

// hintModuleGUC names a variable pg_hint_plan defines; it is a bool only
// when the module is loaded (a placeholder is never listed). A variable so
// tests can stand in a module that is not loaded.
var hintModuleGUC = "pg_hint_plan.enable_hint"

const hintModuleSQL = `/* pg_sage tuner */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_settings
WHERE name = $1 AND vartype = 'bool')`

// ensureHintModule reports whether pg_hint_plan is loaded in this backend,
// LOADing it into pg_sage's own backend first when allowed. A failed LOAD
// aborts the transaction, so it is an error.
func ensureHintModule(ctx context.Context, tx pgx.Tx, sessionLoad bool) (bool, error) {
	var loaded bool
	if err := tx.QueryRow(ctx, hintModuleSQL, hintModuleGUC).Scan(&loaded); err != nil {
		return false, fmt.Errorf("read pg_hint_plan module state: %w", err)
	}
	if loaded || !sessionLoad {
		return loaded, nil
	}
	if _, err := tx.Exec(ctx, "LOAD 'pg_hint_plan'"); err != nil {
		return false, fmt.Errorf("LOAD 'pg_hint_plan' into pg_sage's session: %w", err)
	}
	if err := tx.QueryRow(ctx, hintModuleSQL, hintModuleGUC).Scan(&loaded); err != nil {
		return false, fmt.Errorf("read pg_hint_plan module state: %w", err)
	}
	return loaded, nil
}

// hintSessionSQL turns comment hints on for this transaction only, keeps
// installed hint_plan.hints rows out of both plans (an installed hint must
// not plan the "plain" side), and gives the plans the session's own
// parallel setting (catalogread turns parallel workers off; EXPLAIN without
// ANALYZE launches none).
const hintSessionSQL = `/* pg_sage tuner */ SELECT
	set_config('pg_hint_plan.enable_hint', 'on', true),
	set_config('pg_hint_plan.enable_hint_table', 'off', true),
	set_config('max_parallel_workers_per_gather',
		(SELECT reset_val FROM pg_catalog.pg_settings
		  WHERE name = 'max_parallel_workers_per_gather'), true)`

func serverMajor(ctx context.Context, tx pgx.Tx) (int, error) {
	var num string
	if err := tx.QueryRow(ctx, "SELECT current_setting('server_version_num')").
		Scan(&num); err != nil {
		return 0, fmt.Errorf("read server_version_num: %w", err)
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, fmt.Errorf("parse server_version_num %q: %w", num, err)
	}
	return n / 10000, nil
}

// explainJSON runs one validated EXPLAIN over the simple protocol: the
// extended protocol rejects the unbound $n that GENERIC_PLAN plans.
func explainJSON(ctx context.Context, tx pgx.Tx, sql string) ([]byte, error) {
	results, err := tx.Conn().PgConn().Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		return nil, errors.New("expected one JSON plan row")
	}
	return results[0].Rows[0][0], nil
}

// refuseExplain classifies a failed EXPLAIN: a statement or lock timeout
// (or the caller's deadline) is explain_timeout, anything else
// explain_failed.
func refuseExplain(cmp HintComparison, side string, err error) (HintComparison, error) {
	var pgErr *pgconn.PgError
	timeout := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "55P03"))
	verdict := HintExplainFailed
	if timeout {
		verdict = HintExplainTimeout
	}
	return refuse(cmp, verdict, fmt.Sprintf("%s EXPLAIN: %v", side, err))
}

func refuse(cmp HintComparison, v HintVerdict, reason string) (HintComparison, error) {
	cmp.Verdict, cmp.Reason = v, reason
	return cmp, fmt.Errorf("%w: %s: %s", ErrHintRefused, v, reason)
}

func unverifiable(cmp HintComparison, reason string) HintComparison {
	cmp.Verdict, cmp.Reason = HintUnverifiable, reason
	return cmp
}
