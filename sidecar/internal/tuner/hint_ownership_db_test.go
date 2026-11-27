package tuner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// tuning-02: one hint per statement is enforced from durable state
// (sage.query_hints and hint_plan.hints), not from the in-memory cooldown
// map, and a hint pg_sage did not install is never overwritten.

func ownershipTuner(pool *pgxpool.Pool) *Tuner {
	return New(pool, TunerConfig{WorkMemMaxMB: 256, CascadeCooldownCycles: 3,
		MinQueryCalls: 1, PlanTimeRatio: 0.5},
		&HintPlanAvailability{ApplicationEffect: true, HintTableReady: true}, noopLog2)
}

func ownershipHint(qid int64, hint string) HintProposal {
	return HintProposal{QueryID: qid, Query: "SELECT * FROM t WHERE a = $1",
		Hint: hint, Rationale: "the planner prefers a sequential scan"}
}

// spillHintCandidate is a statement the deterministic rules prescribe a
// hint for without a cached plan: this cycle sized a stat-based temp spill
// for it, so it gets Set(work_mem ...). Until tuning-11 these tests used a
// planning-time-dominated statement and its Set(plan_cache_mode ...) hint;
// that rule now gives parameterization advice and no hint.
func spillHintCandidate(tu *Tuner, qid int64) candidate {
	c := candidate{QueryID: qid, Query: "SELECT * FROM t WHERE a = $1",
		Calls: 100, MeanExecTime: 1}
	if tu.spill.needs == nil {
		tu.spill.needs = map[spillKey]spillSizing{}
	}
	tu.spill.needs[keyOf(c)] = spillSizing{DeltaCalls: 100, DeltaBlocks: 800,
		BlockSize: 8192, PerCallKB: 64, BaselineKB: 4096, NeedMB: 64, CeilingMB: 256,
		HintMB: 64}
	return c
}

// tuneLocked runs the deterministic per-candidate pass the way Tune does,
// under the tuner's lock.
func tuneLocked(
	ctx context.Context, tu *Tuner, cs ...candidate,
) ([]analyzer.Finding, error) {
	tu.mu.Lock()
	defer tu.mu.Unlock()
	return tu.tuneCandidates(ctx, cs, nil)
}

func sageHintStatuses(t *testing.T, pool *pgxpool.Pool, qid int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status FROM sage.query_hints
		WHERE queryid = $1 ORDER BY id`, qid)
	if err != nil {
		t.Fatalf("read sage.query_hints: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func findingFor(fs []analyzer.Finding, qid int64) []analyzer.Finding {
	var out []analyzer.Finding
	for _, f := range fs {
		if f.Category == "query_tuning" && f.Detail["queryid"] == qid {
			out = append(out, f)
		}
	}
	return out
}

// State transition (failed before tuning-02): H1 is active, the map holds
// another statement and Q's cooldown has expired, so the map alone let a
// second hint through.
func TestHintOwnership_ActiveHintBlocksAfterCooldown(t *testing.T) {
	const qid, other = int64(9_230_001), int64(9_230_002)
	pool, ctx := hintScopeDB(t, qid, other)
	recordSageHint(t, pool, qid, scopeOwnHint, "active")
	installHintRow(t, pool, qid, "", scopeOwnHint)
	tu := ownershipTuner(pool)
	tu.recordTuned(other) // a non-empty map is never reloaded
	if _, err := tu.CheckHint(ctx, ownershipHint(qid, "SeqScan(t)")); !errors.Is(err,
		ErrHintExists) {
		t.Fatalf("CheckHint with an active hint = %v, want ErrHintExists", err)
	}
	if err := tu.RecordHint(ctx, ownershipHint(qid, "SeqScan(t)")); !errors.Is(err,
		ErrHintExists) {
		t.Fatalf("RecordHint with an active hint = %v, want ErrHintExists", err)
	}
	if got := sageHintStatuses(t, pool, qid); len(got) != 1 || got[0] != "active" {
		t.Fatalf("sage.query_hints = %v, want only [active]", got)
	}
}

// State transitions of one statement: every durable state that holds or
// installs a hint refuses a new one; a rolled-back hint frees the statement.
func TestHintOwnership_DurableStatesRefuse(t *testing.T) {
	const qid = int64(9_230_101)
	cases := []struct {
		name      string
		status    string
		installed bool
		want      error
	}{
		{"active installed", "active", true, ErrHintExists},
		{"dormant installed", "dormant", true, ErrHintExists},
		{"active, row not yet reconciled away", "active", false, ErrHintExists},
		{"proposed", "proposed", false, ErrHintExists},
		{"retired, removal pending", "retired", true, ErrHintExists},
		{"broken, removal pending", "broken", true, ErrHintExists},
		{"rolled back", "rolled_back", false, nil},
		{"retired and removed", "retired", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx := hintScopeDB(t, qid)
			recordSageHint(t, pool, qid, scopeOwnHint, tc.status)
			if tc.installed {
				installHintRow(t, pool, qid, "", scopeOwnHint)
			}
			_, err := ownershipTuner(pool).CheckHint(ctx, ownershipHint(qid, "SeqScan(t)"))
			if tc.want == nil && err != nil {
				t.Fatalf("CheckHint = %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("CheckHint = %v, want %v", err, tc.want)
			}
		})
	}
}

// Invalid input: an operator's row for Q (any application). The agent's
// hint is refused with ErrForeignHint, and the deterministic pass emits an
// informational finding with no SQL and records nothing.
func TestHintOwnership_OperatorHintRefused(t *testing.T) {
	for _, app := range []string{"", "psql"} {
		t.Run("app="+app, func(t *testing.T) {
			const qid = int64(9_230_011)
			pool, ctx := hintScopeDB(t, qid)
			installHintRow(t, pool, qid, app, scopeOperatorHint)
			tu := ownershipTuner(pool)
			_, err := tu.CheckHint(ctx, ownershipHint(qid, scopeOwnHint))
			if !errors.Is(err, ErrForeignHint) || errors.Is(err, ErrHintExists) {
				t.Fatalf("CheckHint over an operator hint = %v, want ErrForeignHint", err)
			}
			if !strings.Contains(err.Error(), "9230011") {
				t.Fatalf("refusal %q does not name the queryid", err)
			}
			if err := tu.RecordHint(ctx, ownershipHint(qid, scopeOwnHint)); !errors.Is(err,
				ErrForeignHint) {
				t.Fatalf("RecordHint over an operator hint = %v, want ErrForeignHint", err)
			}
			fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid))
			if err != nil {
				t.Fatalf("tuneCandidates: %v", err)
			}
			assertOperatorHintFinding(t, findingFor(fs, qid))
			if got := sageHintStatuses(t, pool, qid); len(got) != 0 {
				t.Fatalf("operator-hinted statement recorded in sage.query_hints: %v", got)
			}
			if got := installedHints(t, pool, qid); len(got) != 1 ||
				got[0] != app+":"+scopeOperatorHint {
				t.Fatalf("installed = %v, want only the operator's", got)
			}
		})
	}
}

func assertOperatorHintFinding(t *testing.T, fs []analyzer.Finding) {
	t.Helper()
	if len(fs) != 1 {
		t.Fatalf("query_tuning findings = %+v, want exactly one", fs)
	}
	f := fs[0]
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("operator-hinted statement got SQL: %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	if f.Severity != "info" || !strings.Contains(f.Title, "operator hint present") {
		t.Fatalf("finding severity=%q title=%q, want info 'operator hint present'",
			f.Severity, f.Title)
	}
	if f.Detail["operator_hint_present"] != true {
		t.Fatalf("detail = %v, want operator_hint_present=true", f.Detail)
	}
}

// State transitions of the deterministic pass: a fresh proposal, its
// refresh after the cooldown (in place, never a second row), and no new
// proposal once the hint is installed.
func TestHintOwnership_TuneProposesOncePerStatement(t *testing.T) {
	const qid = int64(9_230_021)
	pool, ctx := hintScopeDB(t, qid)
	tu := ownershipTuner(pool)
	fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid))
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	got := findingFor(fs, qid)
	if len(got) != 1 || !strings.HasPrefix(got[0].RecommendedSQL, "INSERT INTO hint_plan.hints") {
		t.Fatalf("first pass findings = %+v, want one executable hint", got)
	}
	hint := got[0].Detail["hint_directive"].(string)
	if s := sageHintStatuses(t, pool, qid); len(s) != 1 || s[0] != "proposed" {
		t.Fatalf("after proposal = %v, want [proposed]", s)
	}
	delete(tu.recentlyTuned, qid) // the cooldown expires
	if _, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid)); err != nil {
		t.Fatalf("refresh pass: %v", err)
	}
	if s := sageHintStatuses(t, pool, qid); len(s) != 1 || s[0] != "proposed" {
		t.Fatalf("after refresh = %v, want one [proposed] row", s)
	}
	if n := execRows(t, pool, got[0].RecommendedSQL); n != 1 {
		t.Fatalf("apply inserted %d rows, want 1", n)
	}
	tu.reconcileHintStatuses(ctx)
	delete(tu.recentlyTuned, qid)
	fs, err = tuneLocked(ctx, tu, spillHintCandidate(tu, qid))
	if err != nil {
		t.Fatalf("pass after install: %v", err)
	}
	if got := findingFor(fs, qid); len(got) != 0 {
		t.Fatalf("installed statement re-proposed: %+v", got)
	}
	if s := sageHintStatuses(t, pool, qid); len(s) != 1 || s[0] != "active" {
		t.Fatalf("after install = %v, want only [active]", s)
	}
	if h := installedHints(t, pool, qid); len(h) != 1 || h[0] != ":"+hint {
		t.Fatalf("installed = %v, want [:%s]", h, hint)
	}
}

// Error propagation: the apply INSERT over an existing row fails with the
// unique violation instead of succeeding as a no-op, and the rollback built
// for it deletes nothing (boundary).
func TestHintOwnership_ApplyOverExistingRowFailsLoudly(t *testing.T) {
	const qid = int64(9_230_031)
	pool, ctx := hintScopeDB(t, qid)
	installHintRow(t, pool, qid, "", scopeOperatorHint)
	_, err := pool.Exec(ctx, BuildInsertSQL(qid, scopeOwnHint))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("apply over an existing row = %v, want SQLSTATE 23505", err)
	}
	if n := execRows(t, pool, BuildDeleteSQL(qid, scopeOwnHint)); n != 0 {
		t.Fatalf("rollback deleted %d rows, want 0", n)
	}
	if got := installedHints(t, pool, qid); len(got) != 1 || got[0] != ":"+scopeOperatorHint {
		t.Fatalf("installed = %v, want only the operator's", got)
	}
}

// Nil/empty: without a ready hint table no hint_plan lookup runs and no
// lookup error escapes; a nil pool or no candidates reads nothing.
func TestHintOwnership_NoHintTable(t *testing.T) {
	const qid = int64(9_230_041)
	pool, ctx := noHintTableDB(t)
	recordSageHint(t, pool, qid, scopeOwnHint, "proposed")
	tu := New(pool, TunerConfig{MinQueryCalls: 1, PlanTimeRatio: 0.5},
		&HintPlanAvailability{ApplicationEffect: true}, noopLog2)
	if tu.HintsAvailable() {
		t.Fatal("HintsAvailable without a hint table")
	}
	if _, err := tu.CheckHint(ctx, ownershipHint(qid, "SeqScan(t)")); !errors.Is(err,
		ErrHintsUnavailable) {
		t.Fatalf("CheckHint = %v, want ErrHintsUnavailable", err)
	}
	states, err := tu.hintStates(ctx, []int64{qid, qid + 1})
	if err != nil {
		t.Fatalf("hintStates without a hint table: %v", err)
	}
	if states[qid] != hintProposed || states[qid+1] != hintNone {
		t.Fatalf("states = %v, want %d proposed and %d none", states, qid, qid+1)
	}
	fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid+1))
	if err != nil {
		t.Fatalf("tuneCandidates without a hint table: %v", err)
	}
	if got := findingFor(fs, qid+1); len(got) != 1 || got[0].RecommendedSQL != "" {
		t.Fatalf("findings = %+v, want one advisory finding without SQL", got)
	}
	for _, ids := range [][]int64{nil, {}} {
		if s, err := tu.hintStates(ctx, ids); err != nil || len(s) != 0 {
			t.Fatalf("hintStates(%v) = %v, %v; want empty", ids, s, err)
		}
	}
	if s, err := New(nil, TunerConfig{}, nil, noopLog2).hintStates(ctx,
		[]int64{qid}); err != nil || len(s) != 0 {
		t.Fatalf("hintStates with a nil pool = %v, %v; want empty", s, err)
	}
}

// noHintTableDB is a disposable database without pg_hint_plan or its hint
// table, bootstrapped with the sage schema.
func noHintTableDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "tuning02_nohint"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, s := range []string{"DROP EXTENSION IF EXISTS pg_hint_plan CASCADE",
		"DROP SCHEMA IF EXISTS hint_plan CASCADE"} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// Error propagation: a failed durable lookup refuses the hint (fail
// closed) with the cause in the error, and fails the deterministic pass.
func TestHintOwnership_LookupErrorFailsClosed(t *testing.T) {
	const qid = int64(9_230_051)
	pool, ctx := noHintTableDB(t) // detected as ready, then gone
	tu := ownershipTuner(pool)
	_, err := tu.CheckHint(ctx, ownershipHint(qid, "SeqScan(t)"))
	if err == nil || errors.Is(err, ErrHintExists) ||
		!strings.Contains(err.Error(), "hint state") {
		t.Fatalf("CheckHint with a failed lookup = %v, want a hint state error", err)
	}
	if _, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid)); err == nil ||
		!strings.Contains(err.Error(), "hint_plan.hints") {
		t.Fatalf("tuneCandidates with a failed lookup = %v, want the cause", err)
	}
	if got := sageHintStatuses(t, pool, qid); len(got) != 0 {
		t.Fatalf("a failed lookup recorded %v", got)
	}
}

// Concurrent access: the agent's RecordHint and the deterministic pass on
// the same statement leave exactly one proposed row, in either order.
func TestHintOwnership_ConcurrentRecordAndTune(t *testing.T) {
	const qid = int64(9_230_061)
	for i := 0; i < 8; i++ {
		pool, ctx := hintScopeDB(t, qid)
		tu := ownershipTuner(pool)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var recErr, tuneErr error
		c := spillHintCandidate(tu, qid)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			recErr = tu.RecordHint(ctx, ownershipHint(qid, "SeqScan(t)"))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, tuneErr = tuneLocked(ctx, tu, c)
		}()
		close(start)
		wg.Wait()
		if recErr != nil && !errors.Is(recErr, ErrHintExists) {
			t.Fatalf("round %d: RecordHint = %v", i, recErr)
		}
		if tuneErr != nil {
			t.Fatalf("round %d: tuneCandidates = %v", i, tuneErr)
		}
		if got := sageHintStatuses(t, pool, qid); len(got) != 1 || got[0] != "proposed" {
			t.Fatalf("round %d: sage.query_hints = %v, want one [proposed]", i, got)
		}
	}
}

// Two tuners (a restart, or a second sidecar on the same database) share
// no map: the durable state alone refuses the second recorder.
func TestHintOwnership_SecondTunerSeesDurableProposal(t *testing.T) {
	const qid = int64(9_230_071)
	pool, ctx := hintScopeDB(t, qid)
	if err := ownershipTuner(pool).RecordHint(ctx, ownershipHint(qid, "SeqScan(t)")); err != nil {
		t.Fatalf("first RecordHint: %v", err)
	}
	second := ownershipTuner(pool)
	second.recordTuned(qid + 1)
	if err := second.RecordHint(ctx, ownershipHint(qid, "IndexScan(t)")); !errors.Is(err,
		ErrHintExists) {
		t.Fatalf("second RecordHint = %v, want ErrHintExists", err)
	}
	if got := sageHintStatuses(t, pool, qid); len(got) != 1 {
		t.Fatalf("sage.query_hints = %v, want one row", got)
	}
}
