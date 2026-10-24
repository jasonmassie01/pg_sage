package receipt

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/value"
)

// CHECK-RCP-06: the credit moves only through changereview's state
// machine. A concurred review is pending (15 minutes, toil model 1); a
// clean receipt makes it final; an edited or overridden verdict earns
// none; a regressed or expired one is void. The value ledger counts only
// final minutes as hours and shows pending minutes as potential.

func TestCreditFollowsTheVerdictAndTheReceipt(t *testing.T) {
	f := newFixture(t)
	r := f.seed(nil)
	got, err := f.svc.Decide(f.ctx, r.ID, changereview.DBAVerdict{
		Verdict: changereview.DBAConcur, Actor: "user:1"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CreditState != changereview.CreditPending || got.CreditedMinutes != 15 ||
		got.ToilModelVersion == nil || *got.ToilModelVersion != 1 {
		t.Fatalf("after concur: credit %s/%d model %v", got.CreditState,
			got.CreditedMinutes, got.ToilModelVersion)
	}
	at := time.Now().Add(-10 * time.Minute)
	if _, err := f.svc.ReportDeployed(f.ctx, changereview.Caller{Actor: "mcp:token:ci",
		Source: changereview.SourceMCP}, changereview.Deployment{ID: r.ID,
		DeployedAt: at}); err != nil {
		t.Fatal(err)
	}
	if got = f.get(r.ID); got.CreditState != changereview.CreditPending {
		t.Fatalf("landing moved the credit to %s; only the receipt settles it",
			got.CreditState)
	}
	for _, v := range []changereview.DBAVerdict{
		{Verdict: changereview.DBAConcurWithEdits, Actor: "user:1"},
		{Verdict: changereview.DBAOverride, Actor: "user:1", Reason: "too strict",
			Direction: changereview.DirectionTooStrict}} {
		x := f.seed(nil)
		got, err := f.svc.Decide(f.ctx, x.ID, v)
		if err != nil || got.CreditState != changereview.CreditNone ||
			got.CreditedMinutes != 0 {
			t.Fatalf("%s: credit %s/%d (%v), want none", v.Verdict, got.CreditState,
				got.CreditedMinutes, err)
		}
	}
}

// The job's clean receipt makes the credit final, and the value ledger
// reports it: by_feature.change_review is final minutes / 60, pending
// minutes are potential, and a database that cannot be read is listed as
// unavailable (the report is partial, not failed).
func TestCreditReachesTheValueLedger(t *testing.T) {
	f := newFixture(t)
	f.exec(`DELETE FROM sage.action_log`)
	f.exec(`DELETE FROM sage.action_queue`)
	r, at := f.landedReview(changereview.VerdictSafe, 801)
	f.beforeAfter(801, at, at, 10, 5, 5)
	pending := f.seed(concurred)
	f.run()
	if got := f.get(r.ID); got.CreditState != changereview.CreditFinal {
		t.Fatalf("credit %s, want final", got.CreditState)
	}
	broken := closedPool(t)
	svc := value.NewFleetService(func() []value.Source {
		return []value.Source{{Name: f.name, Pool: f.pool}, {Name: "down", Pool: broken}}
	})
	rep, err := svc.Get(f.ctx, value.Filter{})
	if err != nil {
		t.Fatalf("value report: %v", err)
	}
	if got := rep.ByFeature["change_review"]; math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("by_feature.change_review = %v, want 0.25 (15 final minutes)", got)
	}
	if math.Abs(rep.DBAHoursSaved.AllTime-0.25) > 1e-9 ||
		math.Abs(rep.DBAHoursSaved.ThisWeek-0.25) > 1e-9 {
		t.Fatalf("hours saved %+v, want 0.25 all time and this week", rep.DBAHoursSaved)
	}
	if math.Abs(rep.PotentialHoursPending-0.25) > 1e-9 {
		t.Fatalf("potential_hours_pending = %v, want 0.25 for review %s",
			rep.PotentialHoursPending, pending.ID)
	}
	if !rep.Partial || len(rep.Unavailable) != 1 || rep.Unavailable[0] != "down" {
		t.Fatalf("partial %v unavailable %v, want the closed database listed",
			rep.Partial, rep.Unavailable)
	}
}

// closedPool is a pool on the test server that is already closed.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	return pool
}

// A receipt decided while a DBA decides: the credit lands where the two
// facts together put it, whichever transition commits first.
func TestCreditWithAVerdictAfterTheReceipt(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-25 * time.Hour).UTC().Truncate(time.Microsecond)
	r := f.seed(func(r *changereview.Review) {
		landedAt(r, changereview.LandingSourceReported, at, at)
		watching(r, []string{"rv.rv_orders"}, 802)
	})
	f.beforeAfter(802, at, at, 10, 5, 5)
	f.run()
	got, err := f.svc.Decide(f.ctx, r.ID, changereview.DBAVerdict{
		Verdict: changereview.DBAConcur, Actor: "user:1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.CreditState != changereview.CreditFinal || got.CreditFinalAt == nil {
		t.Fatalf("concur after a clean receipt: credit %s, want final", got.CreditState)
	}
	if _, err := f.svc.Decide(f.ctx, r.ID, changereview.DBAVerdict{
		Verdict: changereview.DBAConcur, Actor: "user:2"}); !errors.Is(err,
		changereview.ErrAlreadyDecided) {
		t.Fatalf("second verdict: %v, want ErrAlreadyDecided", err)
	}
}
