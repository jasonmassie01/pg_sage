//go:build cgo

package impact

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
)

func (e *env) evaluate(pcs []changereview.PostCondition) []PostResult {
	e.t.Helper()
	got, err := EvaluatePostConditions(e.ctx, catalogread.New(e.f.Pool, testTimeouts),
		pcs)
	if err != nil {
		e.t.Fatalf("EvaluatePostConditions: %v", err)
	}
	if len(got) != len(pcs) {
		e.t.Fatalf("%d results for %d post-conditions", len(got), len(pcs))
	}
	return got
}

// CHECK-IMP-14: every DDL kind of the corpus has a post-condition that
// reads not landed before the DDL runs and landed after it.
func TestPostConditionsLandWhenTheDDLRuns(t *testing.T) {
	e := newEnv(t)
	for i, c := range corpus {
		a := e.analyze(c.sql)
		pcs := a.Expectations.PostConditions
		if c.post == "" {
			if len(pcs) != 0 {
				t.Errorf("%d %q: not DDL but post-conditions %+v", i, c.sql, pcs)
			}
			e.f.Exec(applicable(c.sql))
			continue
		}
		if len(pcs) != 1 || pcs[0].Kind != c.post || pcs[0].StatementIndex != 1 {
			t.Fatalf("%d %q: post-conditions %+v, want one %s", i, c.sql, pcs, c.post)
		}
		if got := Landing(e.evaluate(pcs)); got != PostNotLanded {
			t.Fatalf("%d %q: before the DDL the landing reads %q, want not_landed", i,
				c.sql, got)
		}
		e.f.Exec(c.sql)
		if got := e.evaluate(pcs); Landing(got) != PostLanded {
			t.Fatalf("%d %q: after the DDL %+v, want landed", i, c.sql, got)
		}
	}
}

// applicable makes a corpus statement that is not DDL runnable as a test
// statement (a parameter gets a value).
func applicable(sql string) string { return strings.ReplaceAll(sql, "$1", "10") }

func TestFailedUniqueIndexBuildIsInvalid(t *testing.T) {
	e := newEnv(t)
	const sql = "CREATE UNIQUE INDEX CONCURRENTLY rv_orders_status_key ON rv.rv_orders (status)"
	a := e.analyze(sql)
	pcs := a.Expectations.PostConditions
	if len(pcs) != 1 || pcs[0].Kind != changereview.PostIndexValid {
		t.Fatalf("post-conditions %+v", pcs)
	}
	if _, err := e.f.Pool.Exec(e.ctx, sql); err == nil {
		t.Fatal("the unique build over duplicate statuses must fail")
	}
	got := e.evaluate(pcs)
	if Landing(got) != PostInvalid || got[0].State != PostInvalid {
		t.Fatalf("a failed CONCURRENTLY build reads %+v, want invalid", got)
	}
}

func TestMigrationAlreadyHoldingIsNotApplicable(t *testing.T) {
	e := newEnv(t)
	exp := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN IF NOT EXISTS email text").
		Expectations
	reader := catalogread.New(e.f.Pool, testTimeouts)
	landed, err := AlreadyLanded(e.ctx, reader, exp)
	if err != nil || !landed {
		t.Fatalf("AlreadyLanded = %v, %v; want true", landed, err)
	}
	pending := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN note text").Expectations
	if landed, err := AlreadyLanded(e.ctx, reader, pending); err != nil || landed {
		t.Fatalf("a column still to add: AlreadyLanded = %v, %v; want false", landed, err)
	}
	svc := e.service(nil)
	r, _, err := svc.Submit(e.ctx, changereview.Caller{Actor: "test", Source: "cli"},
		changereview.Request{SQL: "ALTER TABLE rv.rv_orders ADD COLUMN IF NOT EXISTS " +
			"email text", WaitSeconds: 30})
	if err != nil || r.State != changereview.StateComplete {
		t.Fatalf("Submit: %v (state %s)", err, r.State)
	}
	if landed, err = AlreadyLanded(e.ctx, reader, r.Expectations); err != nil || !landed {
		t.Fatalf("stored expectations: AlreadyLanded = %v, %v", landed, err)
	}
	// The review's own check at review time makes it not applicable at
	// completion; no receipts cycle has to find it.
	if r.LandingState != changereview.LandingNotApplicable ||
		r.Expectations.AtReview != changereview.AtReviewLanded {
		t.Fatalf("completed review: landing %s at review %q, want not_applicable",
			r.LandingState, r.Expectations.AtReview)
	}
}

// service is a started change-review service whose analyzer is the engine.
func (e *env) service(mod func(*changereview.Deps)) *changereview.Service {
	e.t.Helper()
	d := changereview.Deps{Database: e.f.Name, Pool: e.f.Pool, Analyzer: e.eng,
		Config: input("").Config, Logf: e.t.Logf, DeterministicTimeout: 20 * time.Second,
		ModelTimeout: 5 * time.Second}
	if mod != nil {
		mod(&d)
	}
	svc, err := changereview.New(d)
	if err != nil {
		e.t.Fatalf("changereview.New: %v", err)
	}
	if _, err := svc.Start(e.ctx); err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	return svc
}
