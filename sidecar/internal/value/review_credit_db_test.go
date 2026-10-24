package value

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Change Review credits in the value ledger (CHECK-RCP-06): final review
// minutes count as hours (by_feature change_review, the period totals,
// by_database, trend_daily), read through idx_change_review_credit; minutes
// of reviews a DBA concurred with whose receipt is pending are potential.
// Void, none and pending minutes never count as hours.

const reviewRow = `INSERT INTO sage.change_review (database_name, source, actor,
	input_hash, state, stage, verdict, deterministic_verdict, parse_mode, model_status,
	landing_state, landing_source, landed_lower, landed_at, receipt_verdict, dba_verdict,
	dba_actor, dba_decided_at, credit_state, credited_minutes, toil_model_version,
	credit_final_at, completed_at)
	VALUES ('db', 'mcp', 'mcp:token:ci', md5(random()::text) || md5(random()::text),
	'complete', 'done', 'safe', 'safe', 'ast', 'no_model', $1,
	CASE WHEN $1 = 'landed' THEN 'reported' END,
	CASE WHEN $1 = 'landed' THEN now() - interval '2 days' END,
	CASE WHEN $1 = 'landed' THEN now() - interval '2 days' END, $2, 'concur', 'user:1',
	now() - interval '3 days', $3, $4, 1, $5, now() - interval '3 days')`

// seedReviewCredit inserts one concurred review in landing state landing
// with receipt receipt and the given credit.
func seedReviewCredit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, landing,
	receipt, credit string, minutes int, finalAt *time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, reviewRow, landing, receipt, credit, minutes,
		finalAt); err != nil {
		t.Fatalf("seed review credit: %v", err)
	}
}

func TestReviewCreditsCountOnlyFinalMinutes(t *testing.T) {
	ctx := context.Background()
	src := newLedgerSource(t, "reviews")
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -40)
	seedCredit(t, ctx, src.Pool, "create_index_concurrently", 45)
	seedReviewCredit(t, ctx, src.Pool, "landed", "clean", "final", 15, &now)
	seedReviewCredit(t, ctx, src.Pool, "landed", "clean_low_traffic", "final", 15, &old)
	seedReviewCredit(t, ctx, src.Pool, "awaiting", "pending", "pending", 15, nil)
	seedReviewCredit(t, ctx, src.Pool, "landed", "pending", "pending", 15, nil)
	seedReviewCredit(t, ctx, src.Pool, "landed", "regressed", "void", 0, nil)
	seedReviewCredit(t, ctx, src.Pool, "not_applicable", "pending", "none", 0, nil)
	svc := NewFleetService(func() []Source { return []Source{src} })
	rep, err := svc.Get(ctx, Filter{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := rep.ByFeature["change_review"]; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("by_feature.change_review = %v, want 0.5 (two final reviews)", got)
	}
	if math.Abs(rep.DBAHoursSaved.AllTime-1.25) > 1e-9 ||
		math.Abs(rep.DBAHoursSaved.ThisWeek-1.0) > 1e-9 {
		t.Fatalf("hours saved %+v, want 1.25 all time, 1.0 this week", rep.DBAHoursSaved)
	}
	if len(rep.ByDatabase) != 1 || math.Abs(rep.ByDatabase[0].Hours-1.25) > 1e-9 {
		t.Fatalf("by_database %+v, want reviews 1.25", rep.ByDatabase)
	}
	if math.Abs(rep.PotentialHoursPending-0.5) > 1e-9 {
		t.Fatalf("potential_hours_pending = %v, want 0.5 (two pending reviews)",
			rep.PotentialHoursPending)
	}
	if !hasDay(rep.TrendDaily, old.Format("2006-01-02"), 0.25) {
		t.Fatalf("trend_daily %+v has no 0.25 h on %s", rep.TrendDaily,
			old.Format("2006-01-02"))
	}
	week, err := svc.Get(ctx, Filter{Since: now.AddDate(0, 0, -7)})
	if err != nil {
		t.Fatal(err)
	}
	if got := week.ByFeature["change_review"]; math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("windowed by_feature.change_review = %v, want 0.25", got)
	}
}

func hasDay(days []DayHours, day string, hours float64) bool {
	for _, d := range days {
		if d.Day == day && math.Abs(d.Hours-hours) < 1e-9 {
			return true
		}
	}
	return false
}

// The final-credit read is an index range of idx_change_review_credit,
// never a scan of the review table.
func TestReviewCreditReadUsesTheCreditIndex(t *testing.T) {
	ctx := context.Background()
	src := newLedgerSource(t, "review_plan")
	if _, err := src.Pool.Exec(ctx, `INSERT INTO sage.change_review (database_name, source,
		actor, input_hash, state) SELECT 'db', 'mcp', 'a', md5(g::text) || md5(g::text),
		'failed' FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedReviewCredit(t, ctx, src.Pool, "landed", "clean", "final", 15, &now)
	if err := testdb.VacuumAllVisible(ctx, src.Pool, "sage.change_review",
		60*time.Second); err != nil {
		t.Fatal(err)
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	plan, err := testdb.Explain(ctx, src.Pool, "", reviewCreditSQL, nil, nil, month,
		startOfWeek(now))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SeqScans("change_review") != 0 || !plan.Has(func(n testdb.PlanNode) bool {
		return n.Index == "idx_change_review_credit"
	}) {
		t.Fatalf("final-credit read plan:\n%s", plan)
	}
	for _, sql := range []string{reviewPendingSQL} {
		p, err := testdb.Explain(ctx, src.Pool, "", sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.SeqScans("change_review") != 0 {
			t.Fatalf("pending-credit read scans the review table:\n%s", p)
		}
	}
}
