package receipt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

// CHECK-RCP-01 and CHECK-RCP-02: a completed review waits to land. It
// lands when its catalog post-conditions hold (bracketed between the last
// check that saw it not landed and the check that saw it landed), when a
// signed change event links its PR, or when a deploy is reported. One
// already satisfied when first checked is not applicable; one that never
// lands expires.

func statusIndexReview(r *changereview.Review, index string) {
	r.Statements = []changereview.Statement{{Index: 1, Origin: changereview.OriginSQLInput,
		SQL:  "CREATE INDEX CONCURRENTLY " + index + " ON rv.rv_orders (status)",
		Kind: "create_index", Relations: []string{rvfixture.Orders}, EvidenceID: "stmt:1"}}
	r.Expectations.PostConditions = []changereview.PostCondition{{StatementIndex: 1,
		Kind: changereview.PostIndexValid, Relation: rvfixture.Orders, Index: index}}
	r.Expectations.Relations = []string{rvfixture.Orders}
}

func (f *fixture) checkedAt(id string) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT landing_checked_at FROM sage.change_review
		WHERE id = $1::uuid`, id).Scan(&at); err != nil {
		f.t.Fatalf("read landing_checked_at: %v", err)
	}
	return at
}

func TestLandingByCatalogBracketsTheDDL(t *testing.T) {
	rv := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newFixtureOn(t, ctx, rv.Pool, rv.Name)
	r := f.seed(func(r *changereview.Review) { statusIndexReview(r, "rv_orders_status_idx") })

	rep := f.run()
	if got := f.get(r.ID); got.LandingState != changereview.LandingAwaiting ||
		rep.Evaluated != 1 || rep.Landed != 0 {
		t.Fatalf("before the DDL: landing %s, report %+v", got.LandingState, rep)
	}
	checked := f.checkedAt(r.ID)
	if checked == nil {
		t.Fatal("the check that saw the change not landed was not recorded")
	}

	ddlStart := time.Now()
	rv.Exec("CREATE INDEX CONCURRENTLY rv_orders_status_idx ON rv.rv_orders (status)")
	ddlEnd := time.Now()

	rep = f.run()
	got := f.get(r.ID)
	if rep.Landed != 1 || got.LandingState != changereview.LandingLanded ||
		got.LandingSource != changereview.LandingSourceCatalog {
		t.Fatalf("after the DDL: landing %s/%s, report %+v", got.LandingState,
			got.LandingSource, rep)
	}
	if !got.LandedLower.Before(ddlStart) || got.LandedAt.Before(ddlEnd) {
		t.Fatalf("bracket [%s, %s] does not contain the DDL [%s, %s]", got.LandedLower,
			got.LandedAt, ddlStart, ddlEnd)
	}
	if !got.LandedLower.Equal(checked.UTC()) {
		t.Fatalf("landed_lower %s, want the last not-landed check %s", got.LandedLower,
			checked)
	}
	if got.ReceiptVerdict != changereview.ReceiptPending {
		t.Fatalf("receipt %s, want pending until the window closes", got.ReceiptVerdict)
	}
	if rep = f.run(); rep.Landed != 0 || rep.Evaluated != 0 {
		t.Fatalf("a landed review was evaluated again: %+v", rep)
	}
}

// A post-condition that already holds the first time it is checked means
// the change was there before the review: nothing lands, nothing credits.
func TestLandingAlreadySatisfiedIsNotApplicable(t *testing.T) {
	rv := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newFixtureOn(t, ctx, rv.Pool, rv.Name)
	r := f.seed(func(r *changereview.Review) {
		statusIndexReview(r, "rv_orders_created_idx")
		concurred(r)
	})
	rep := f.run()
	got := f.get(r.ID)
	if rep.NotApplicable != 1 || got.LandingState != changereview.LandingNotApplicable {
		t.Fatalf("landing %s, report %+v; want not_applicable", got.LandingState, rep)
	}
	if got.CreditState != changereview.CreditNone || got.CreditedMinutes != 0 {
		t.Fatalf("credit %s/%d, want none: nothing landed", got.CreditState,
			got.CreditedMinutes)
	}
}

// A CONCURRENTLY build that failed leaves an invalid index: the change did
// not land.
func TestLandingInvalidIndexKeepsWaiting(t *testing.T) {
	rv := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newFixtureOn(t, ctx, rv.Pool, rv.Name)
	r := f.seed(func(r *changereview.Review) { statusIndexReview(r, "rv_orders_status_uidx") })
	f.run()
	if _, err := rv.Pool.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY rv_orders_status_uidx "+
		"ON rv.rv_orders (status)"); err == nil {
		t.Fatal("the duplicate-key build succeeded; the fixture needs duplicate statuses")
	}
	rep := f.run()
	if got := f.get(r.ID); got.LandingState != changereview.LandingAwaiting ||
		rep.Landed != 0 || rep.Evaluated != 1 {
		t.Fatalf("an invalid index landed: %s, %+v", got.LandingState, rep)
	}
}

func TestLandingBySignedChangeEvent(t *testing.T) {
	f := newFixture(t)
	r := f.seed(func(r *changereview.Review) { r.PRURL = testPR })
	at := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Microsecond)
	f.event(changefeed.KindMigration, testPR, at)
	rep := f.run()
	got := f.get(r.ID)
	if rep.Landed != 1 || got.LandingState != changereview.LandingLanded ||
		got.LandingSource != changereview.LandingSourceChangeEvent {
		t.Fatalf("landing %s/%s, report %+v", got.LandingState, got.LandingSource, rep)
	}
	if !got.LandedAt.Equal(at) || !got.LandedLower.Equal(at) {
		t.Fatalf("landed [%s, %s], want the event's occurred_at %s", got.LandedLower,
			got.LandedAt, at)
	}
}

// Events that do not anchor: another PR's, a kind that is not a deploy or
// migration, and one that happened before the review existed.
func TestLandingIgnoresEventsThatDoNotAnchor(t *testing.T) {
	f := newFixture(t)
	r := f.seed(func(r *changereview.Review) { r.PRURL = testPR })
	f.event(changefeed.KindDeploy, "https://github.com/acme/shop/pull/8",
		time.Now().Add(-10*time.Minute))
	f.event(changefeed.KindFeatureFlag, testPR, time.Now().Add(-10*time.Minute))
	f.event(changefeed.KindDeploy, testPR, r.CreatedAt.Add(-time.Minute))
	rep := f.run()
	if got := f.get(r.ID); got.LandingState != changereview.LandingAwaiting ||
		rep.Landed != 0 {
		t.Fatalf("landing %s, report %+v; no event anchors this review",
			got.LandingState, rep)
	}
}

// A deploy of a PR lands the revision it shipped (the newest complete
// review before the event) and every awaiting revision with the same
// content, not a superseded revision with other content.
func TestLandingByEventLandsTheShippedRevision(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	shipped := randomHex(64)
	old := f.seed(func(r *changereview.Review) {
		r.PRURL, r.CreatedAt, r.CompletedAt = testPR, now.Add(-5*time.Hour),
			ptr(now.Add(-5*time.Hour+time.Minute))
	})
	same := f.seed(func(r *changereview.Review) {
		r.PRURL, r.ContentHash, r.CreatedAt, r.CompletedAt = testPR, shipped,
			now.Add(-4*time.Hour), ptr(now.Add(-4*time.Hour+time.Minute))
	})
	newest := f.seed(func(r *changereview.Review) {
		r.PRURL, r.ContentHash, r.CreatedAt, r.CompletedAt = testPR, shipped,
			now.Add(-3*time.Hour), ptr(now.Add(-3*time.Hour+time.Minute))
	})
	f.event(changefeed.KindDeploy, testPR, now.Add(-time.Hour))
	rep := f.run()
	if rep.Landed != 2 {
		t.Fatalf("report %+v, want the shipped revision and its twin landed", rep)
	}
	for _, id := range []string{same.ID, newest.ID} {
		if got := f.get(id); got.LandingSource != changereview.LandingSourceChangeEvent {
			t.Fatalf("review %s: landing %s/%s", id, got.LandingState, got.LandingSource)
		}
	}
	if got := f.get(old.ID); got.LandingState != changereview.LandingAwaiting {
		t.Fatalf("the superseded revision landed: %s", got.LandingState)
	}
}

func TestLandingByReportedDeploy(t *testing.T) {
	f := newFixture(t)
	r := f.seed(nil)
	at := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Microsecond)
	if _, err := f.svc.ReportDeployed(f.ctx, changereview.Caller{Actor: "mcp:token:ci",
		Source: changereview.SourceMCP}, changereview.Deployment{ID: r.ID,
		DeployedAt: at}); err != nil {
		t.Fatalf("ReportDeployed: %v", err)
	}
	rep := f.run()
	got := f.get(r.ID)
	if got.LandingSource != changereview.LandingSourceReported || !got.LandedAt.Equal(at) ||
		got.ReceiptVerdict != changereview.ReceiptPending || rep.Decided != 0 {
		t.Fatalf("reported landing %s/%s at %v, receipt %s, report %+v",
			got.LandingState, got.LandingSource, got.LandedAt, got.ReceiptVerdict, rep)
	}
}

// landing_expiry_days after completion, an unlanded review expires: the
// receipt is unverifiable and sealed, the credit void. A day short, it
// still waits.
func TestLandingExpiresAfterLandingExpiryDays(t *testing.T) {
	f := newFixture(t)
	r := f.seed(concurred)
	expiry := time.Duration(f.cfg.LandingExpiryDays) * 24 * time.Hour
	f.clock.advance(r.CompletedAt.Sub(time.Now()) + expiry - time.Minute)
	if rep := f.run(); rep.Expired != 0 {
		t.Fatalf("expired a minute early: %+v", rep)
	}
	if got := f.get(r.ID); got.LandingState != changereview.LandingAwaiting {
		t.Fatalf("landing %s a minute before expiry", got.LandingState)
	}
	f.clock.advance(2 * time.Minute)
	rep := f.run()
	got := f.get(r.ID)
	if rep.Expired != 1 || got.LandingState != changereview.LandingExpired ||
		got.ReceiptVerdict != changereview.ReceiptUnverifiable ||
		got.CreditState != changereview.CreditVoid {
		t.Fatalf("after expiry: landing %s receipt %s credit %s, report %+v",
			got.LandingState, got.ReceiptVerdict, got.CreditState, rep)
	}
	if got.ReceiptHash == "" || !changereview.VerifyReceipt(got) {
		t.Fatal("the expired receipt is not sealed")
	}
}

// A malformed post-condition fails its own review only: the others in the
// same batched catalog read still land.
func TestLandingIsolatesAMalformedPostCondition(t *testing.T) {
	f := newFixture(t)
	good := f.seed(func(r *changereview.Review) {
		r.Expectations.PostConditions = []changereview.PostCondition{{StatementIndex: 1,
			Kind: changereview.PostIndexValid, Relation: "sage.change_review",
			Index: "change_review_created"}}
	})
	bad := f.seed(func(r *changereview.Review) {
		r.Expectations.PostConditions = []changereview.PostCondition{{StatementIndex: 1,
			Kind: "bogus", Relation: "sage.change_review"}}
	})
	rep := f.run()
	if got := f.get(good.ID); got.LandingState != changereview.LandingNotApplicable {
		t.Fatalf("good review landing %s, want not_applicable", got.LandingState)
	}
	if got := f.get(bad.ID); got.LandingState != changereview.LandingAwaiting {
		t.Fatalf("malformed review landing %s, want awaiting", got.LandingState)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], bad.ID) {
		t.Fatalf("errors %v, want one naming review %s", rep.Errors, bad.ID)
	}
}
