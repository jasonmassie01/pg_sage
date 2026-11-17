package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// review-01a wiring: the grade runs once the startup first look is done,
// records the first grade's time for /metrics, and stays off when
// review.enabled is false.

func reviewRuntime(t *testing.T, ctx context.Context, name string,
	enabled bool) *databaseRuntime {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Review.Enabled = enabled
	cfg.SchemaLint.Enabled = false
	pool := clusterPool(t, ctx, "review_"+name)
	return &databaseRuntime{spec: databaseRuntimeSpec{Name: name, Pool: pool, Scope: name},
		ctx: ctx, workers: &sync.WaitGroup{}, cfg: cfg, provider: "postgres"}
}

func TestReviewGradesDB_FirstGradeAfterTheFirstLookAndTheMetric(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := reviewRuntime(t, ctx, "review_wired", true)
	t.Cleanup(func() { cancel(); rt.workers.Wait() })
	done := make(chan struct{})
	started := time.Now()
	rt.startReviewGrades(done, started)
	time.Sleep(200 * time.Millisecond)
	store := rubric.NewStore(rt.spec.Pool)
	if runs, _ := store.Latest(ctx, "review_wired", 1); len(runs) != 0 {
		t.Fatal("graded before the startup first look ended")
	}
	close(done)
	var runs []rubric.Run
	deadline := time.Now().Add(30 * time.Second)
	for len(runs) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		runs, _ = rubric.NewStore(rt.spec.Pool).Latest(ctx, "review_wired", 1)
	}
	if len(runs) != 1 || runs[0].Triggers[0] != "first_look" ||
		runs[0].Grade.RubricVersion != rubric.Version || len(runs[0].Grade.Dimensions) != 8 {
		t.Fatalf("runs = %+v, want the first_look grade", runs)
	}
	if took := time.Since(started); took > 10*time.Minute {
		t.Fatalf("first grade after %s, want under 10 minutes", took)
	}
	var b strings.Builder
	writeReviewMetrics(&b, reviewFirstGrades)
	want := `pg_sage_review_first_grade_seconds{database="review_wired"}`
	if !strings.Contains(b.String(), want) {
		t.Fatalf("metrics = %q, want the first grade of review_wired", b.String())
	}
	if !strings.Contains(strings.Join(rt.features, ","), "review") {
		t.Fatalf("features = %v, want review noted", rt.features)
	}
}

func TestReviewGrades_DisabledStartsNothing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Review.Enabled = false
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "off"}, ctx: context.Background(),
		workers: &sync.WaitGroup{}, cfg: cfg}
	rt.startReviewGrades(make(chan struct{}), time.Now())
	if len(rt.features) != 0 {
		t.Fatalf("features = %v, want nothing started", rt.features)
	}
}

// No concurrent access tests beyond the lock: record keeps the first value.
func TestFirstGrades_RecordKeepsTheFirstAndForgetClears(t *testing.T) {
	f := &firstGrades{secs: map[string]float64{}}
	f.record("a", 3*time.Second)
	f.record("a", 9*time.Second)
	var b strings.Builder
	writeReviewMetrics(&b, f)
	if !strings.Contains(b.String(), `{database="a"} 3`) ||
		!strings.Contains(b.String(), "# TYPE pg_sage_review_first_grade_seconds gauge") {
		t.Fatalf("metrics = %q, want a's first grade at 3 s", b.String())
	}
	f.forget("a")
	b.Reset()
	writeReviewMetrics(&b, f)
	if b.Len() != 0 {
		t.Fatalf("after forget = %q, want nothing", b.String())
	}
	if letterOrUnknown("") != "unknown" || letterOrUnknown("B") != "B" {
		t.Fatal("letterOrUnknown")
	}
}
