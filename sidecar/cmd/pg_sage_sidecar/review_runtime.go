package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/review/trigger"
)

// Sage Review's triggers (review-04a): wiring. Each database runtime with
// review.enabled registers its trigger queue (sage.review_trigger) with
// trigger.Default for the request hooks, polls the stored sources, and on
// the leader starts a review run (kind review) for the triggers that are
// due. A run lists its trigger kinds in sage.review_run.triggers, and
// each trigger row names the run that consumed it ("why now").

// The loop's timing; tests shorten them.
var (
	reviewTriggerFirst = 2 * time.Minute
	reviewTriggerTick  = 5 * time.Minute
)

const (
	// reviewTriggerSourceEvery spaces the polls of the stored sources.
	reviewTriggerSourceEvery = time.Hour
	// reviewTriggerLease outlives the reaper's stale-run bound, so a
	// claim is never reclaimed while its run may still be running.
	reviewTriggerLease = rubric.StaleRunAfter + 5*time.Minute
)

// startReviewTriggers starts the trigger loop.
func (rt *databaseRuntime) startReviewTriggers() {
	if !rt.cfg.Review.Enabled {
		return
	}
	store := trigger.NewStore(rt.spec.Pool)
	sched := rt.reviewScheduler(store)
	trigger.Default.Register(rt.spec.Name, store, rt.cfg.Review.LargeTableBytes)
	rt.start(func() {
		defer trigger.Default.Unregister(rt.spec.Name, store)
		everyAfter(rt.ctx, reviewTriggerFirst, reviewTriggerTick, func(ctx context.Context) {
			if fleetLeader != nil && !fleetLeader.IsLeader() {
				return
			}
			if _, err := sched.Tick(ctx); err != nil && ctx.Err() == nil {
				logWarn(rt.spec.Scope, "db %q: review triggers: %v", rt.spec.Name, err)
			}
		})
	})
	rt.note("review_triggers")
}

// reviewScheduler is the database's trigger loop and its sources.
func (rt *databaseRuntime) reviewScheduler(store *trigger.Store) *trigger.Scheduler {
	name := rt.spec.Name
	hist := histstore.Resolve(rt.spec.Pool)
	sources := []trigger.Source{
		trigger.EnrollmentSource(store, hist, name),
		trigger.CalendarSource(store, name, rt.cfg.Review.CalendarDays),
		trigger.RegressionSource(store, name),
		trigger.CostGrowthSource(hist, name),
		trigger.InstanceSource(store, name, rt.reviewInstanceFacts),
	}
	return &trigger.Scheduler{Store: store, Database: name, Owner: reviewTriggerOwner(),
		Interval: time.Duration(rt.cfg.Review.MinIntervalHours) * time.Hour,
		Lease:    reviewTriggerLease, Reviewer: rt.reviewTriggerReviewer(),
		Sources: sources, SourceEvery: reviewTriggerSourceEvery,
		Log: func(level, format string, args ...any) {
			logStructured(level, rt.spec.Scope, format, args...)
		}}
}

// reviewTriggerOwner names this sidecar on its claims: the leader's holder
// id when there is one, else host and process.
func reviewTriggerOwner() string {
	if fleetLeader != nil && fleetLeader.Holder() != "" {
		return fleetLeader.Holder()
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// rubricReviewer starts a review run through the rubric runner: today a
// review run is graded against the rubric, read-only, and carries its
// triggers.
type rubricReviewer struct{ base rubric.Runner }

func (rt *databaseRuntime) reviewTriggerReviewer() trigger.Reviewer {
	return rubricReviewer{base: rubric.Runner{Store: rubric.NewStore(rt.spec.Pool),
		Rubric: rubric.V1(), Database: rt.spec.Name, Loader: rt.reviewLoader()}}
}

func (r rubricReviewer) Review(ctx context.Context, run trigger.Run) (int64, error) {
	runner := r.base
	runner.OnStart = run.OnStart
	out, err := runner.Run(ctx, rubric.KindReview, run.Kinds())
	if errors.Is(err, rubric.ErrRunning) {
		return 0, fmt.Errorf("%w: %v", trigger.ErrBusy, err)
	}
	return out.RunID, err
}

// reviewInstanceFacts are the instance's size facts from cloud telemetry;
// without a cloud adapter they are unavailable (never guessed).
func (rt *databaseRuntime) reviewInstanceFacts(context.Context) (trigger.Facts, bool,
	error) {
	if rt.cloud == nil {
		return trigger.Facts{}, false, nil
	}
	s := rt.cloud.Status().Sample
	if s == nil || s.Serverless {
		return trigger.Facts{}, false, nil
	}
	f := trigger.Facts{Class: s.InstanceClass, VCPUs: s.VCPUs, Source: s.Provider}
	if s.MemoryTotalBytes != nil {
		f.MemoryBytes = int64(s.MemoryTotalBytes.Value)
	}
	if f == (trigger.Facts{Source: s.Provider}) {
		return trigger.Facts{}, false, nil
	}
	return f, true, nil
}

const (
	reviewTriggerErrorsMetric = "pg_sage_review_trigger_fire_errors_total"
	reviewRunsMetric          = "pg_sage_review_runs_total"
)

// writeReviewTriggerMetrics exports the failed trigger inserts by kind and
// the trigger-started runs by cause.
func writeReviewTriggerMetrics(b *strings.Builder) {
	if errs := trigger.FireErrors(); len(errs) > 0 {
		kinds := make([]string, 0, len(errs))
		for k := range errs {
			kinds = append(kinds, string(k))
		}
		sort.Strings(kinds)
		metricsreg.Header(b, reviewTriggerErrorsMetric)
		for _, k := range kinds {
			writeSample(b, reviewTriggerErrorsMetric, []labelPair{{"kind", k}},
				errs[trigger.Kind(k)])
		}
		b.WriteString("\n")
	}
	if runs := trigger.RunsStarted(); len(runs) > 0 {
		metricsreg.Header(b, reviewRunsMetric)
		for _, cause := range []string{"calendar", "event"} {
			if n, ok := runs[cause]; ok {
				writeSample(b, reviewRunsMetric, []labelPair{{"cause", cause}}, n)
			}
		}
		b.WriteString("\n")
	}
}
