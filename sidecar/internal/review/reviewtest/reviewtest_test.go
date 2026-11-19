package reviewtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// T6 test round 1: the helpers' own tests (covfloor's 50% helper floor).
// The seeding SQL itself runs on real PostgreSQL in every scorecard and
// model-gate database test that seeds through it; here a recording fake
// checks what is written and that every error names its step.
// No concurrent-access test: the helpers keep no shared state.

func TestWindowEndsAtTheFixedInstant(t *testing.T) {
	w := Window(7)
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !w.To.Equal(want) || !w.From.Equal(want.AddDate(0, 0, -7)) {
		t.Fatalf("Window(7) = %+v", w)
	}
	if z := Window(0); !z.From.Equal(z.To) {
		t.Fatalf("Window(0) = %+v, want an empty window", z)
	}
}

func TestBuildSealsOnlyTheSectionsGiven(t *testing.T) {
	bare, err := Build(Spec{Database: "orders", Window: Window(7)})
	if err != nil {
		t.Fatalf("Build bare: %v", err)
	}
	full, err := Build(Spec{Database: "orders", Window: Window(7),
		Texts: &workpack.Texts{}, Indexes: &workpack.Indexes{}, Schema: &workpack.Schema{},
		Settings: &workpack.Settings{}, Stats: &workpack.Stats{}, Plans: &workpack.Plans{},
		Growth: &workpack.Growth{DBSize: workpack.Series{ID: "growth/db/size"}},
		PGVersion: "17.2", Platform: "self-managed"})
	if err != nil {
		t.Fatalf("Build full: %v", err)
	}
	names := func(p *workpack.Pack) string {
		var out []string
		for _, f := range p.Manifest.Files {
			out = append(out, f.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(bare); got != "coverage.json,growth.json,header.json,workload.json" {
		t.Fatalf("bare pack files = %s", got)
	}
	for _, want := range []string{"texts.json", "indexes.json", "schema.json",
		"settings.json", "stats.json", "plans.json"} {
		if !strings.Contains(names(full), want) {
			t.Errorf("full pack lacks %s: %s", want, names(full))
		}
	}
	if strings.Contains(names(full), "provider.json") {
		t.Fatalf("a nil provider section was sealed: %s", names(full))
	}
	if bare.Manifest.ContentSHA256 == full.Manifest.ContentSHA256 {
		t.Fatal("two different packs have one content hash")
	}
}

// fakeDB records every statement; fail makes the statement whose text
// contains it fail.
type fakeDB struct {
	stmts []string
	args  [][]any
	fail  string
	next  int64
}

type fakeRow struct {
	id  int64
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64) = r.id
	return nil
}

func (d *fakeDB) record(sql string, args []any) error {
	d.stmts, d.args = append(d.stmts, sql), append(d.args, args)
	if d.fail != "" && strings.Contains(sql, d.fail) {
		return errors.New("connection refused")
	}
	return nil
}

func (d *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.record(sql, args)
}

func (d *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	d.next++
	return fakeRow{id: d.next, err: d.record(sql, args)}
}

func TestSeedReviewWritesTheRunModelsPlanAndSteps(t *testing.T) {
	db := &fakeDB{}
	run, err := SeedReview(context.Background(), db, Review{Database: "orders",
		Models: []string{"m1", "m2"}, Steps: []Outcome{
			{Origin: "generator", Ref: " idx ", Accepted: true, Verdict: "improved"},
			{Origin: "model", Ref: "m1", Reverted: true, ActionType: "drop_index",
				SQL: "DROP INDEX CONCURRENTLY i"},
			{Origin: "person", Ref: "alice", Accepted: true},
			{Origin: "generator", Ref: "x"}}})
	if err != nil || run != 1 {
		t.Fatalf("SeedReview = %d, %v", run, err)
	}
	// run, 2 model runs, plan, review_plan, 4 x (step, origin)
	if len(db.stmts) != 13 {
		t.Fatalf("%d statements, want 13: %v", len(db.stmts), db.stmts)
	}
	if db.args[0][1] != "done" || db.args[3][0] != "model" {
		t.Fatalf("run status %v, plan origin %v; want done and model", db.args[0][1],
			db.args[3][0])
	}
	states := []string{}
	for i, s := range db.stmts {
		if strings.Contains(s, "INSERT INTO sage.plan_step (") {
			states = append(states, db.args[i][4].(string))
		}
	}
	if strings.Join(states, ",") != "kept,reverted,judging,pending" {
		t.Fatalf("step states = %v", states)
	}
	if last := db.args[len(db.args)-3]; last[4] != "alice" {
		t.Fatalf("person origin ref = %v", last[4])
	}
}

func TestSeedReviewWithoutStepsWritesNoPlan(t *testing.T) {
	db := &fakeDB{}
	run, err := SeedReview(context.Background(), db, Review{Database: "d",
		Status: "running"})
	if err != nil || run != 1 || len(db.stmts) != 1 || db.args[0][1] != "running" {
		t.Fatalf("SeedReview = %d, %v after %v", run, err, db.stmts)
	}
}

func TestSeedReviewErrorsNameTheStep(t *testing.T) {
	steps := []Outcome{{Origin: "generator", Ref: "g"}}
	for fail, want := range map[string]string{
		"sage.review_run ":        "seed a review run",
		"sage.review_model_run":   "seed a model run",
		"INSERT INTO sage.plan (": "seed a plan",
		"sage.review_plan":        "seed a review plan",
		"sage.plan_step (":        "seed plan step 1",
		"sage.plan_step_origin":   "seed the origin of step 1",
	} {
		_, err := SeedReview(context.Background(), &fakeDB{fail: fail},
			Review{Database: "d", Models: []string{"m"}, Steps: steps})
		if err == nil || !strings.Contains(err.Error(), want) ||
			!strings.Contains(err.Error(), "connection refused") {
			t.Errorf("failing %q: err = %v, want %q", fail, err, want)
		}
	}
}
