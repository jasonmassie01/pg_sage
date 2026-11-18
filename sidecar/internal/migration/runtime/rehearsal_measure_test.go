package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/tuning"
)

// lab-04b: choosing and pairing the affected statements. No concurrent
// access tests here: affected and pairQuery share no state (two
// rehearsals at once are covered in rehearsal/receipt_test.go).

const seqPlan = `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"users","Schema":"public",` +
	`"Total Cost":4180}}]`
const indexPlan = `[{"Plan":{"Node Type":"Index Scan","Relation Name":"users",` +
	`"Schema":"public","Index Name":"users_email_idx","Total Cost":8.3}}]`

func executed(latency float64, blocks int64, plan string) side {
	return side{got: tuning.CloneMeasurement{Cost: 1, LatencyMS: latency, Blocks: &blocks,
		Plan: []byte(plan)}}
}

func planned(cost float64, plan string) side {
	return side{got: tuning.CloneMeasurement{Cost: cost, Plan: []byte(plan),
		NotExecuted: "not executed: parameters unknown"}}
}

func TestPairQueryBothExecuted(t *testing.T) {
	q := pairQuery(7, executed(0.05, 4, indexPlan), executed(14.2, 2210, seqPlan))
	if q.QueryID != 7 || q.BeforeLatencyMS != 0.05 || q.AfterLatencyMS != 14.2 ||
		*q.BeforeBlocks != 4 || *q.AfterBlocks != 2210 || q.Unmeasured != "" ||
		q.NotExecuted != "" {
		t.Fatalf("query = %#v", q)
	}
	if q.BeforePlanHash == "" || q.BeforePlanHash == q.AfterPlanHash {
		t.Fatalf("plan hashes %q and %q, want two different", q.BeforePlanHash,
			q.AfterPlanHash)
	}
}

// Like with like: executed before but planned after compares plan cost
// and drops the latency.
func TestPairQueryMixedSidesComparesCost(t *testing.T) {
	q := pairQuery(7, executed(0.05, 4, indexPlan), planned(4180, seqPlan))
	if q.BeforeLatencyMS != 0 || q.BeforeBlocks != nil || q.AfterLatencyMS != 0 ||
		q.BeforeCost != 1 || q.AfterCost != 4180 ||
		q.NotExecuted != "not executed: parameters unknown" {
		t.Fatalf("query = %#v", q)
	}
}

// Boundary: a timeout on either side is recorded and leaves the
// statement unmeasured, never a regression.
func TestPairQueryTimeouts(t *testing.T) {
	timedOut := side{err: tuning.ErrStatementTimedOut}
	before := pairQuery(1, timedOut, side{skipped: true})
	if !before.TimedOut || before.Unmeasured != rehearsalpkg.UnmeasuredTimedOutBefore {
		t.Fatalf("before timeout = %#v", before)
	}
	after := pairQuery(2, executed(40, 9, indexPlan), timedOut)
	if !after.TimedOut || after.Unmeasured != rehearsalpkg.UnmeasuredTimedOutAfter ||
		after.BeforeLatencyMS != 40 || after.AfterLatencyMS != 0 {
		t.Fatalf("after timeout = %#v", after)
	}
}

// Invalid input: more than one statement is skipped with the reason; an
// unreadable plan and other errors name what failed.
func TestPairQueryInvalidInput(t *testing.T) {
	q := pairQuery(3, side{err: fmt.Errorf("wrap: %w", tuning.ErrNotOneStatement)}, side{})
	if q.Unmeasured != rehearsalpkg.UnmeasuredNotOne || q.TimedOut {
		t.Fatalf("multi = %#v", q)
	}
	q = pairQuery(4, planned(5, `[]`), planned(5, indexPlan))
	if !strings.HasPrefix(q.Unmeasured, "plan hash: ") {
		t.Fatalf("bad plan = %#v", q)
	}
	q = pairQuery(5, planned(5, indexPlan), side{err: errors.New("connection reset")})
	if q.Unmeasured != rehearsalpkg.UnmeasuredFailedAfter+": connection reset" {
		t.Fatalf("after error = %#v", q)
	}
}

func statements(n int) []AffectedStatement {
	out := make([]AffectedStatement, n)
	for i := range out {
		out[i] = AffectedStatement{QueryID: int64(i + 1),
			Text: fmt.Sprintf("SELECT id FROM public.users WHERE id = %d", i)}
	}
	return out
}

func runnerWith(source WorkloadSource) *PostgresRehearsalRunner {
	return NewPostgresRehearsalRunner(0, 0).WithWorkload(source, 0)
}

var users = planpkg.TableFacts{Schema: "public", Name: "users"}

// Boundary: 20 statements are measured, the 21st is not.
func TestAffectedCapsAtTwenty(t *testing.T) {
	stmts, note := runnerWith(func(context.Context, planpkg.TableFacts) (
		[]AffectedStatement, error) {
		return statements(21), nil
	}).affected(context.Background(), users)
	if len(stmts) != MaxAffectedStatements || MaxAffectedStatements != 20 || note != "" ||
		stmts[19].QueryID != 20 {
		t.Fatalf("got %d statements, note %q", len(stmts), note)
	}
}

// Nil/empty and error propagation: each empty workload says why.
func TestAffectedNamesEveryEmptyWorkload(t *testing.T) {
	absent := fmt.Errorf("%w: extension is not installed", ErrNoStatementTexts)
	for name, test := range map[string]struct {
		runner *PostgresRehearsalRunner
		table  planpkg.TableFacts
		want   string
	}{
		"no source": {NewPostgresRehearsalRunner(0, 0), users, "no production workload"},
		"no table":  {runnerWith(nil), planpkg.TableFacts{}, "no production workload"},
		"none":      {runnerWith(sourceOf(nil, nil)), users, NoStatementReferences},
		"no pgss":   {runnerWith(sourceOf(nil, absent)), users, absent.Error()},
		"read failure": {runnerWith(sourceOf(nil, errors.New("timeout"))), users,
			"read the production workload: timeout"},
		"unnamed table": {runnerWith(sourceOf(statements(1), nil)), planpkg.TableFacts{},
			"the plan names no table"},
	} {
		stmts, note := test.runner.affected(context.Background(), test.table)
		if len(stmts) != 0 || !strings.HasPrefix(note, test.want) {
			t.Fatalf("%s: %d statements, note %q, want %q", name, len(stmts), note, test.want)
		}
	}
	if NoStatementReferences == absent.Error() {
		t.Fatal("the two empty workloads must read differently")
	}
}

func sourceOf(stmts []AffectedStatement, err error) WorkloadSource {
	return func(context.Context, planpkg.TableFacts) ([]AffectedStatement, error) {
		return stmts, err
	}
}
