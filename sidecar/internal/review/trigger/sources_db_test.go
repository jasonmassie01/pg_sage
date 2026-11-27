package trigger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// The sources on a real server, each from a seeded producer table.

// seedCoverage writes one coverage row per hour for hours hours ending at
// the hour before now, skipping every hour i with skip(i).
func seedCoverage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, now time.Time,
	hours, keep int) {
	t.Helper()
	end := now.Truncate(time.Hour)
	_, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_coverage
		(hour, sampled_exec_ms, sampled_queryids, epoch_breaks, snapshots, epoch_source)
		SELECT $1::timestamptz - make_interval(hours => g), 0, 0, 0, 1, 'recorded'
		  FROM generate_series(1, $2::int) g WHERE g = $2 OR g <= $3`, end, hours, keep)
	if err != nil {
		t.Fatalf("seed coverage: %v", err)
	}
}

func evaluate(t *testing.T, ctx context.Context, src Source, now time.Time) []Trigger {
	t.Helper()
	got, err := src.Evaluate(ctx, now)
	if err != nil {
		t.Fatalf("%s: %v", src.Kind(), err)
	}
	return got
}

// Boundary and nil/empty: 13 days of rollups give no enrollment; 14 days
// at 79% give none and at 80% give one.
func TestSourcesDB_EnrollmentNeedsTwoWeeksAtEightyPercent(t *testing.T) {
	now := time.Now().UTC()
	for _, c := range []struct {
		label       string
		hours, keep int
		want        int
	}{
		{"none", 0, 0, 0},
		{"thirteen", 13 * 24, 13 * 24, 0},
		{"seventynine", 336, 264, 0},
		{"eighty", 336, 268, 1},
	} {
		t.Run(c.label, func(t *testing.T) {
			pool, ctx := livePool(t, "enroll_"+c.label)
			if c.hours > 0 {
				seedCoverage(t, ctx, pool, now, c.hours, c.keep)
			}
			src := EnrollmentSource(NewStore(pool), histstore.Resolve(pool), "shop")
			got := evaluate(t, ctx, src, now)
			if len(got) != c.want {
				t.Fatalf("%d enrollment triggers, want %d: %+v", len(got), c.want, got)
			}
			if c.want == 1 && (got[0].Kind != KindEnrollment ||
				got[0].Detail["hours_covered"] != 269) {
				t.Fatalf("trigger %+v, want enrollment with 269 hours", got[0])
			}
		})
	}
}

func seedReview(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
	finished time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version, status, started_at, finished_at)
		VALUES ($1, 'review', 'v', 'done', $2, $2) RETURNING id`, db, finished).
		Scan(&id); err != nil {
		t.Fatalf("seed review: %v", err)
	}
	return id
}

func TestSourcesDB_CalendarBoundaryAndNoEnrollmentAfterAReview(t *testing.T) {
	pool, ctx := livePool(t, "calendar")
	s := NewStore(pool)
	now := time.Now().UTC().Truncate(time.Second)
	run := seedReview(t, ctx, pool, "shop", now.Add(-(29*24*time.Hour + 23*time.Hour)))
	cal := CalendarSource(s, "shop", 30)
	if got := evaluate(t, ctx, cal, now); len(got) != 0 {
		t.Fatalf("calendar at 29d23h: %+v", got)
	}
	if got := evaluate(t, ctx, cal, now.Add(time.Hour)); len(got) != 1 ||
		got[0].Key != fmt.Sprintf("after:%d", run) {
		t.Fatalf("calendar at 30d: %+v, want one keyed after run %d", got, run)
	}
	seedCoverage(t, ctx, pool, now, 336, 336)
	enr := EnrollmentSource(s, histstore.Resolve(pool), "shop")
	if got := evaluate(t, ctx, enr, now); len(got) != 0 {
		t.Fatalf("enrollment after a full review: %+v", got)
	}
}

func seedRegression(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ident string,
	age time.Duration, outcome, verdict string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (created_at, category,
		severity, object_identifier, title, detail) VALUES (now() - $1::interval,
		'query_regression', 'warning', $2, 'regressed', '{}') RETURNING id`,
		fmt.Sprintf("%d seconds", int64(age.Seconds())), ident).Scan(&id); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if outcome == "" {
		return id
	}
	var action int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type,
		finding_id, sql_executed, outcome) VALUES ('create_index', $1, 'SELECT 1', $2)
		RETURNING id`, id, outcome).Scan(&action); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	if verdict != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.action_outcome (action_log_id,
			action_class, predicted, prediction_method, verdict) VALUES ($1, 'index',
			'{}', 'rule', $2)`, action, verdict); err != nil {
			t.Fatalf("seed outcome: %v", err)
		}
	}
	return id
}

// Only an action verification judged improved resolves a regression: one
// that ran but is unjudged or neutral leaves it unresolved.
func TestSourcesDB_RegressionOpenSeventyTwoHoursWithoutAVerifiedAction(t *testing.T) {
	pool, ctx := livePool(t, "regression")
	none := seedRegression(t, ctx, pool, "q1", 73*time.Hour, "", "")
	reverted := seedRegression(t, ctx, pool, "q2", 80*time.Hour, "rolled_back", "")
	seedRegression(t, ctx, pool, "q3", 71*time.Hour, "", "")                 // too young
	seedRegression(t, ctx, pool, "q4", 100*time.Hour, "success", "improved") // verified
	unjudged := seedRegression(t, ctx, pool, "q5", 90*time.Hour, "success", "")
	neutral := seedRegression(t, ctx, pool, "q6", 90*time.Hour, "success", "neutral")
	got := evaluate(t, ctx, RegressionSource(NewStore(pool), "shop"), time.Now())
	want := []int64{none, reverted, unjudged, neutral}
	reasons := []string{"no verified action", "action reverted", "no verified action",
		"no verified action"}
	if len(got) != len(want) {
		t.Fatalf("regression triggers %+v, want q1, q2, q5 and q6", got)
	}
	for i, id := range want {
		if got[i].Key != fmt.Sprintf("finding:%d", id) || got[i].Detail["reason"] != reasons[i] {
			t.Fatalf("trigger %d = %s %v, want finding:%d %q", i, got[i].Key,
				got[i].Detail["reason"], id, reasons[i])
		}
	}
}

// seedSizes writes one system snapshot a day for vals, the last on the day
// before now.
func seedSizes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, now time.Time,
	vals []int64) {
	t.Helper()
	last := now.UTC().Truncate(24 * time.Hour).Add(-12 * time.Hour)
	for i, v := range vals {
		at := last.AddDate(0, 0, i-len(vals)+1)
		if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category,
			data) VALUES ($1, 'system', jsonb_build_object('db_size_bytes', $2::int8))`,
			at, v); err != nil {
			t.Fatalf("seed size: %v", err)
		}
	}
}

func TestSourcesDB_CostGrowthAnomalyFromDailySizes(t *testing.T) {
	pool, ctx := livePool(t, "growth")
	now := time.Now().UTC()
	vals := steady(15, 1<<30)
	seedSizes(t, ctx, pool, now, vals)
	src := CostGrowthSource(histstore.Resolve(pool), "shop")
	if got := evaluate(t, ctx, src, now); len(got) != 0 {
		t.Fatalf("steady growth fired: %+v", got)
	}
	seedSizes(t, ctx, pool, now.AddDate(0, 0, 1),
		[]int64{vals[len(vals)-1] + 25<<30})
	got := evaluate(t, ctx, src, now.AddDate(0, 0, 1))
	if len(got) != 1 || got[0].Kind != KindCostGrowth || got[0].Detail["z"] == nil {
		t.Fatalf("spike: %+v, want one cost growth trigger with its z", got)
	}
}

func TestSourcesDB_InstanceChangeComparesWithTheLastFacts(t *testing.T) {
	pool, ctx := livePool(t, "instance")
	s := NewStore(pool)
	facts := Facts{Class: "db.r6g.large", VCPUs: 2, MemoryBytes: 16 << 30, Source: "rds"}
	known := true
	src := InstanceSource(s, "shop", func(context.Context) (Facts, bool, error) {
		return facts, known, nil
	})
	if got := evaluate(t, ctx, src, t0); len(got) != 0 {
		t.Fatalf("first look fired: %+v", got)
	}
	if got := evaluate(t, ctx, src, t0.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("unchanged fired: %+v", got)
	}
	known = false
	if got := evaluate(t, ctx, src, t0.Add(2*time.Hour)); len(got) != 0 {
		t.Fatalf("unavailable facts fired: %+v", got)
	}
	known, facts.Class, facts.VCPUs = true, "db.r6g.xlarge", 4
	got := evaluate(t, ctx, src, t0.Add(3*time.Hour))
	if len(got) != 1 || got[0].Detail["to_class"] != "db.r6g.xlarge" ||
		got[0].Detail["from_vcpus"] != 2 {
		t.Fatalf("resize: %+v, want one instance_change from 2 to 4 vCPUs", got)
	}
}
