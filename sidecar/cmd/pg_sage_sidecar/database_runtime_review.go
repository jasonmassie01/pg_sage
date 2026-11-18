package main

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/schema/lint"
)

// Sage Review's grade (review-01a): wiring. Once the startup first look
// has finished, each database runtime grades its database against the
// rubric, read-only, then again every review.grade_interval_hours. The
// time from registration to the first grade is
// pg_sage_review_first_grade_seconds.

const reviewMetric = "pg_sage_review_first_grade_seconds"

// reviewDrillPools names where a database's drill rows live (the control
// database drills record in); unset or nil is the database's own schema.
// The API server sets it once the fleet is known.
var reviewDrillPools atomic.Pointer[func(string) *pgxpool.Pool]

// firstGrades holds each database's seconds to its first grade.
type firstGrades struct {
	mu   sync.Mutex
	secs map[string]float64
}

var reviewFirstGrades = &firstGrades{secs: map[string]float64{}}

// record keeps the first grade of name only.
func (f *firstGrades) record(name string, took time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, done := f.secs[name]; !done {
		f.secs[name] = took.Seconds()
	}
}

func (f *firstGrades) forget(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secs, name)
}

// writeReviewMetrics exports each database's seconds to its first grade,
// then the review trigger counters (review-04a).
func writeReviewMetrics(b *strings.Builder, f *firstGrades) {
	defer writeReviewTriggerMetrics(b)
	f.mu.Lock()
	names := make([]string, 0, len(f.secs))
	for n := range f.secs {
		names = append(names, n)
	}
	secs := make(map[string]float64, len(f.secs))
	for n, s := range f.secs {
		secs[n] = s
	}
	f.mu.Unlock()
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	metricsreg.Header(b, reviewMetric)
	for _, n := range names {
		writeSample(b, reviewMetric, dbLabel(n), strconv.FormatFloat(secs[n], 'f', -1, 64))
	}
	b.WriteString("\n")
}

// startReviewGrades starts the grade loop; it waits for the startup first
// look (done) so the first grade reads it.
func (rt *databaseRuntime) startReviewGrades(done <-chan struct{}, started time.Time) {
	if !rt.cfg.Review.Enabled {
		return
	}
	interval := time.Duration(rt.cfg.Review.GradeIntervalHours) * time.Hour
	run := &rubric.Runner{Store: rubric.NewStore(rt.spec.Pool), Rubric: rubric.V1(),
		Database: rt.spec.Name, Loader: rt.reviewLoader()}
	rt.start(func() {
		select {
		case <-rt.ctx.Done():
			return
		case <-done:
		}
		if rt.firstGrade(run, started) {
			everyAfter(rt.ctx, interval, interval, func(ctx context.Context) {
				rt.gradeOnce(ctx, run, false, started)
			})
		}
		reviewFirstGrades.forget(rt.spec.Name) // a rebuilt runtime measures anew
	})
	rt.note("review")
}

// reviewFirstGradeRetry is how soon a first grade that did not record is
// tried again: a transient error, or the running row of a sidecar killed
// mid-run (reaped after rubric.StaleRunAfter), must not delay the first
// grade by a whole interval.
var reviewFirstGradeRetry = time.Minute

// firstGrade tries the first grade until one is recorded; false when the
// runtime ended first.
func (rt *databaseRuntime) firstGrade(run *rubric.Runner, started time.Time) bool {
	for !rt.gradeOnce(rt.ctx, run, true, started) {
		select {
		case <-rt.ctx.Done():
			return false
		case <-time.After(reviewFirstGradeRetry):
		}
	}
	return true
}

// gradeOnce runs one grade; it reports whether a grade was recorded.
func (rt *databaseRuntime) gradeOnce(ctx context.Context, run *rubric.Runner, first bool,
	started time.Time) bool {
	trigger := "daily"
	if first {
		trigger = "first_look"
	}
	out, err := run.Run(ctx, rubric.KindGrade, []string{trigger})
	switch {
	case errors.Is(err, rubric.ErrRunning):
		logInfo(rt.spec.Scope, "db %q: review grade skipped: another run is running",
			rt.spec.Name)
		return false
	case err != nil:
		logWarn(rt.spec.Scope, "db %q: review grade: %v", rt.spec.Name, err)
		return false
	}
	if first {
		reviewFirstGrades.record(rt.spec.Name, time.Since(started))
	}
	logInfo(rt.spec.Scope, "db %q: review grade %s (%s, coverage %.0f%%, %d changes)",
		rt.spec.Name, letterOrUnknown(out.Grade.Overall), out.Status,
		out.Grade.CoveragePct, len(out.Delta.Dimensions))
	if out.LoadErr != nil {
		logWarn(rt.spec.Scope, "db %q: review grade is partial: %v", rt.spec.Name,
			out.LoadErr)
	}
	return true
}

func letterOrUnknown(l rubric.Letter) string {
	if l == "" {
		return "unknown"
	}
	return string(l)
}

// reviewLoader reads this database's producers: the drill rows where
// drills record them, a live lint scan when schema lint is on, and the
// operator's cost rates.
func (rt *databaseRuntime) reviewLoader() *rubric.DBLoader {
	l := &rubric.DBLoader{Pool: rt.spec.Pool, Database: rt.spec.Name,
		Provider: rt.provider, CostRates: len(rt.cfg.Cost.Rates) > 0,
		LintDisabled: rt.cfg.SchemaLint.DisabledRules, Drills: reviewDrills{rt.spec}}
	if rt.cfg.SchemaLint.Enabled {
		linter := lint.New(rt.spec.Pool, &rt.cfg.SchemaLint, rt.pgVersion(),
			logStructuredWrapper)
		linter.SetCatalogReadTimeouts(catalogread.FromSafety(rt.cfg.Safety))
		l.Lint = linter
	}
	return l
}

// reviewDrills reads the drill history from the store drills record in,
// resolved per read (a control database may connect later).
type reviewDrills struct{ spec databaseRuntimeSpec }

func (d reviewDrills) store() *drill.Store {
	if f := reviewDrillPools.Load(); f != nil && *f != nil {
		if p := (*f)(d.spec.Name); p != nil {
			return drill.NewStore(p)
		}
	}
	return drill.NewStore(d.spec.Pool)
}

func (d reviewDrills) LastPassedResult(ctx context.Context, database string) (
	*drill.Result, error) {
	return d.store().LastPassedResult(ctx, database)
}

func (d reviewDrills) LastAttempt(ctx context.Context, database string) (drill.Attempt,
	bool, error) {
	return d.store().LastAttempt(ctx, database)
}

// setReviewDrillPools points the grades at the drills' results database.
func setReviewDrillPools(c *config.Config, mgr *fleet.DatabaseManager, meta *metaDBState) {
	f := restoreDrillResults(c, mgr, meta)
	reviewDrillPools.Store(&f)
}
