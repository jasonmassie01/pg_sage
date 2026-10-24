package receipt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// The job over the fleet: one database's failure is reported and never
// stops the others; a cancelled cycle stops with the context's error; a
// disabled database is skipped; an unreadable change feed postpones the
// receipt rather than deciding it without its confounders.

func TestCycleContinuesPastAFailingDatabase(t *testing.T) {
	f := newFixture(t)
	r := f.seed(func(r *changereview.Review) { r.PRURL = testPR })
	f.event("migration", testPR, time.Now().Add(-time.Minute))
	broken, err := changereview.New(changereview.Deps{Database: "broken",
		Pool: closedPool(t), Config: f.cfg, Analyzer: nopAnalyzer{}})
	if err != nil {
		t.Fatal(err)
	}
	reports := f.job().Cycle(f.ctx, []Database{
		{Name: "zz-broken", Service: broken, Pool: closedPool(t)}, f.db(),
		{Name: "aa-no-service"}})
	if len(reports) != 3 {
		t.Fatalf("reports %+v, want one per database", reports)
	}
	byName := map[string]Report{}
	for _, rep := range reports {
		byName[rep.Database] = rep
	}
	if byName[f.name].Landed != 1 {
		t.Fatalf("the healthy database was not processed: %+v", byName[f.name])
	}
	if errs := byName["zz-broken"].Errors; len(errs) == 0 ||
		!strings.Contains(errs[0], "awaiting") {
		t.Fatalf("broken database errors %v, want the awaiting read named", errs)
	}
	if errs := byName["aa-no-service"].Errors; len(errs) == 0 {
		t.Fatal("a database without a service reported no error")
	}
	if reports[0].Database != "aa-no-service" || reports[2].Database != "zz-broken" {
		t.Fatalf("reports not in name order: %v, %v, %v", reports[0].Database,
			reports[1].Database, reports[2].Database)
	}
	if got := f.get(r.ID); got.LandingState != changereview.LandingLanded {
		t.Fatalf("landing %s", got.LandingState)
	}
}

func TestRunDatabaseStopsWhenCancelled(t *testing.T) {
	f := newFixture(t)
	f.seed(nil)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err := f.job().RunDatabase(ctx, f.db())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
}

func TestRunDatabaseSkipsADisabledService(t *testing.T) {
	f := newFixture(t)
	f.cfg = config.DefaultChangeReviewConfig()
	f.cfg.Enabled = false
	f.newService()
	r := f.seed(func(r *changereview.Review) { r.PRURL = testPR })
	f.event("deploy", testPR, time.Now().Add(-time.Minute))
	rep := f.run()
	if rep.Evaluated != 0 || rep.Landed != 0 {
		t.Fatalf("a disabled database was processed: %+v", rep)
	}
	if got := f.get(r.ID); got.LandingState != changereview.LandingAwaiting {
		t.Fatalf("landing %s, want untouched", got.LandingState)
	}
}

// failingFeed is a change feed that cannot be read.
type failingFeed struct{}

func (failingFeed) List(context.Context, changefeed.Filter) ([]changefeed.Event, error) {
	return nil, errors.New("change feed store unavailable")
}

func TestReceiptWaitsWhenTheChangeFeedFails(t *testing.T) {
	f := newFixture(t)
	r, at := f.landedReview(changereview.VerdictSafe, 1001)
	f.beforeAfter(1001, at, at, 10, 5, 5)
	db := f.db()
	db.Changes = failingFeed{}
	rep, err := f.job().RunDatabase(f.ctx, db)
	if err != nil {
		t.Fatalf("RunDatabase: %v", err)
	}
	if rep.Decided != 0 || len(rep.Errors) == 0 ||
		!strings.Contains(strings.Join(rep.Errors, ";"), "change feed") {
		t.Fatalf("report %+v, want the receipt postponed with the feed error", rep)
	}
	if got := f.get(r.ID); got.ReceiptVerdict != changereview.ReceiptPending {
		t.Fatalf("receipt %s decided without its confounders", got.ReceiptVerdict)
	}
	db.Changes = nil
	if rep = f.run(); rep.Decided != 1 {
		t.Fatalf("with the feed back: %+v", rep)
	}
}

func TestRunDatabaseRejectsAnIncompleteDatabase(t *testing.T) {
	j := NewJob(Deps{})
	for name, db := range map[string]Database{"no name": {Service: nil},
		"no service": {Name: "x"}} {
		if _, err := j.RunDatabase(context.Background(), db); !errors.Is(err,
			changereview.ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
	if got := j.Cycle(context.Background(), nil); len(got) != 0 {
		t.Fatalf("an empty fleet reported %+v", got)
	}
}
