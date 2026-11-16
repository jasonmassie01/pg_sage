package executor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Verification v2 in the executor (lab-01c): the index watch computes v2
// beside v1 as verify.method resolves for this database (auto is shadow
// until a passing back-test exists), with control statements that do not
// name the changed table.

// verifyControlLimit bounds a watch's control group.
const verifyControlLimit = 12

// controlStatementsSQL is the most-called statements that name neither
// the changed table ($1, a word-boundary regex) nor a target ($2).
func controlStatementsSQL(rel string) string {
	return `/* pg_sage executor */ SELECT s.queryid FROM ` + rel + ` s
	WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
	  AND s.query !~* $1 AND NOT (s.queryid = ANY($2::bigint[])) AND ` +
		workload.AdviceSQL("s.query") + `
	GROUP BY s.queryid ORDER BY sum(s.calls) DESC LIMIT $3`
}

// WithBacktestReader installs the reader of this database's verifier
// back-tests (lab-02): verify.method auto acts on v2 once the latest one
// passes. Without it auto stays in shadow.
func (e *Executor) WithBacktestReader(reader verify.BacktestReader) {
	e.methodResolver = verify.NewMethodResolver(reader, nil)
}

// OperatorVerification is what judges a plan's operator steps on this
// executor's database (plan-02d): v2's readers and the verification
// window (verify.window_minutes, else its default).
func (e *Executor) OperatorVerification() (verify.V2Options, time.Duration) {
	minutes := config.DefaultVerifyWindowMinutes
	if e.cfg != nil && e.cfg.Verify.WindowMinutes > 0 {
		minutes = e.cfg.Verify.WindowMinutes
	}
	return e.verificationV2Options(), time.Duration(minutes) * time.Minute
}

// verificationV2Options wires v2 to this executor's database.
func (e *Executor) verificationV2Options() verify.V2Options {
	if e.methodResolver == nil {
		e.methodResolver = verify.NewMethodResolver(verify.NoBacktest{}, nil)
	}
	cycle := time.Minute
	if e.cfg != nil && e.cfg.Collector.IntervalSeconds > 0 {
		cycle = time.Duration(e.cfg.Collector.IntervalSeconds) * time.Second
	}
	return verify.V2Options{Counters: verify.NewCounterSource(e.pool),
		Mechanisms: verify.NewPostgresObservationSource(e.pool),
		Method:     e.verificationMethod, Controls: e.controlStatements, Cycle: cycle}
}

// verificationMethod is verify.method resolved for this database: v1,
// shadow or v2. A refused or unreadable choice is logged and fails closed
// to what the resolver returned (v1 or shadow, which act on v1).
func (e *Executor) verificationMethod(ctx context.Context) string {
	configured := verify.MethodAuto
	if e.cfg != nil {
		configured = e.cfg.Verify.Method
	}
	if e.methodResolver == nil {
		e.methodResolver = verify.NewMethodResolver(verify.NoBacktest{}, nil)
	}
	res, err := e.methodResolver.Resolve(ctx, configured, e.databaseName)
	if err != nil {
		e.logFn("verify", "verification method for %q: %v (judging by %s)",
			e.databaseName, err, res.Method)
	}
	return res.Method
}

// controlStatements is a watch's control group: the busiest statements
// that do not name its table. None (no pg_stat_statements, or no table)
// leaves the attribution without a control term.
func (e *Executor) controlStatements(ctx context.Context, state verify.WatchState) (
	[]int64, error) {
	parts := splitQualifiedIdentifier(state.Table)
	if e.pool == nil || len(parts) == 0 || parts[len(parts)-1] == "" {
		return nil, nil
	}
	return e.workloadIDs(ctx, controlStatementsSQL, wordPattern(parts[len(parts)-1]),
		state.Criterion.TargetIDs, verifyControlLimit), nil
}

// withVerifier records which verifier decided v and the other's verdict.
func withVerifier(o *verify.Outcome, v verify.Verdict) {
	o.Verifier = v.Verifier
	if v.Other == nil {
		return
	}
	if verify.DecidedVerdict(v.Other.Outcome) {
		o.OtherVerdict = v.Other.Outcome
	}
	o.OtherEvidence = v.Other.Evidence
	if o.OtherEvidence == nil {
		o.OtherEvidence = map[string]any{"reason": v.Other.Reason}
	}
}
