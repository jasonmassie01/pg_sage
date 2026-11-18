package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/review/trigger"
)

// review-04a wiring: the review trigger loop registers the database's
// queue for the source hooks, runs a review run (kind review) listing the
// triggers' kinds, and stays off with review.enabled false.

func fastReviewTriggers(t *testing.T) {
	t.Helper()
	first, tick := reviewTriggerFirst, reviewTriggerTick
	reviewTriggerFirst, reviewTriggerTick = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { reviewTriggerFirst, reviewTriggerTick = first, tick })
}

func TestReviewRuntime_PreUpgradeStartsAReviewRunListingIt(t *testing.T) {
	fastReviewTriggers(t)
	ctx, cancel := context.WithCancel(context.Background())
	rt := reviewRuntime(t, ctx, "review_trig", true)
	t.Cleanup(func() { cancel(); rt.workers.Wait() })
	rt.startReviewTriggers()
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for err = errors.New("unregistered"); err != nil && time.Now().Before(deadline); {
		err = trigger.Default.PreUpgrade(ctx, "review_trig", 18, "pg_upgrade", time.Now())
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("pre_upgrade publish: %v", err)
	}
	var runs []rubric.Run
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ = rubric.NewStore(rt.spec.Pool).Latest(ctx, "review_trig", 1)
		if len(runs) == 1 && runs[0].Status != rubric.RunRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(runs) != 1 || runs[0].Kind != rubric.KindReview ||
		strings.Join(runs[0].Triggers, ",") != "pre_upgrade" ||
		runs[0].Status == rubric.RunFailed || len(runs[0].Grade.Dimensions) != 8 {
		t.Fatalf("runs = %+v, want one graded review run listing pre_upgrade", runs)
	}
	why, err := trigger.NewStore(rt.spec.Pool).WhyNow(ctx, runs[0].ID)
	if err != nil || len(why) != 1 || why[0].Kind != trigger.KindPreUpgrade ||
		why[0].Detail["target_major"] != float64(18) || why[0].State != trigger.StateConsumed {
		t.Fatalf("why now = %+v (%v), want the consumed pre_upgrade", why, err)
	}
	var b strings.Builder
	writeReviewTriggerMetrics(&b)
	if !strings.Contains(b.String(), `pg_sage_review_runs_total{cause="event"}`) {
		t.Fatalf("metrics = %q, want the event-started run counted", b.String())
	}
}

func TestReviewRuntime_DisabledRegistersNoQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := reviewRuntime(t, ctx, "review_trig_off", false)
	t.Cleanup(func() { cancel(); rt.workers.Wait() })
	rt.startReviewTriggers()
	err := trigger.Default.PreUpgrade(ctx, "review_trig_off", 18, "pg_upgrade", time.Now())
	if !errors.Is(err, trigger.ErrUnknownDatabase) {
		t.Fatalf("publish to a disabled review = %v, want ErrUnknownDatabase", err)
	}
}

// A grade already running: the reviewer says busy, so the loop waits.
func TestReviewRuntime_RunningGradeIsBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := reviewRuntime(t, ctx, "review_trig_busy", true)
	if _, err := rt.spec.Pool.Exec(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version) VALUES ('review_trig_busy', 'grade', 'v')`); err != nil {
		t.Fatal(err)
	}
	_, err := rt.reviewTriggerReviewer().Review(ctx, trigger.Run{Database: "review_trig_busy",
		Triggers: []trigger.Trigger{{Kind: trigger.KindCalendar}}})
	if !errors.Is(err, trigger.ErrBusy) {
		t.Fatalf("review while a grade runs = %v, want ErrBusy", err)
	}
}

// Without cloud telemetry the instance facts are unavailable, never a
// guess.
func TestReviewRuntime_InstanceFactsNeedCloudTelemetry(t *testing.T) {
	rt := &databaseRuntime{}
	f, ok, err := rt.reviewInstanceFacts(context.Background())
	if ok || err != nil || f != (trigger.Facts{}) {
		t.Fatalf("facts %+v %v %v, want unavailable", f, ok, err)
	}
}

func TestReviewRuntime_MetricsCountFireErrorsByKind(t *testing.T) {
	_ = trigger.NewHub().PreUpgrade(context.Background(), "nosuch", 18, "", time.Now())
	var b strings.Builder
	writeReviewTriggerMetrics(&b)
	out := b.String()
	if !strings.Contains(out, `pg_sage_review_trigger_fire_errors_total{kind="pre_upgrade"}`) ||
		!strings.Contains(out, "# TYPE pg_sage_review_trigger_fire_errors_total counter") {
		t.Fatalf("metrics = %q, want the pre_upgrade fire error counter", out)
	}
}
