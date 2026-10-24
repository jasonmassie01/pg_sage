package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/receipt"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// CHECK-RCP-01 (leader gate): the receipts job runs only on the fleet's
// leader. On a follower the cycle does nothing and the review is left
// untouched; on the leader the change lands.

type receiptsAnalyzer struct{}

func (receiptsAnalyzer) Analyze(context.Context, changereview.Input) (changereview.Analysis,
	error) {
	return changereview.Analysis{ParseMode: changereview.ParseAST}, nil
}

func receiptsFixture(t *testing.T) (*pgxpool.Pool, *fleet.DatabaseManager,
	*changereview.Registry, changereview.Review) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "cr_receipts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE public.cr_t (a int)`); err != nil {
		t.Fatal(err)
	}
	svc, err := changereview.New(changereview.Deps{Database: "crdb", Pool: pool,
		Config: config.DefaultChangeReviewConfig(), Analyzer: receiptsAnalyzer{}})
	if err != nil {
		t.Fatal(err)
	}
	reg := changereview.NewRegistry()
	reg.Set("crdb", svc)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "crdb", Pool: pool,
		Status: &fleet.InstanceStatus{}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "other", Pool: pool,
		Status: &fleet.InstanceStatus{}})
	done := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	r, err := svc.Store().Insert(ctx, changereview.Review{Database: "crdb",
		Source: changereview.SourceMCP, Actor: "mcp:token:ci",
		InputHash: strings.Repeat("e", 64), State: changereview.StateComplete,
		Verdict: changereview.VerdictSafe, DeterministicVerdict: changereview.VerdictSafe,
		ParseMode: changereview.ParseAST, ModelStatus: changereview.ModelNoModel,
		Expectations: changereview.Expectations{
			PostConditions: []changereview.PostCondition{{StatementIndex: 1,
				Kind: changereview.PostIndexValid, Relation: "public.cr_t",
				Index: "cr_t_idx"}},
			WatchQueryIDs: []int64{}, Relations: []string{"public.cr_t"},
			Predictions: []verify.Prediction{}},
		LandingState: changereview.LandingAwaiting, ReceiptVerdict: changereview.ReceiptPending,
		CreatedAt: done, CompletedAt: &done})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	return pool, mgr, reg, r
}

func setLeader(t *testing.T, held bool) {
	t.Helper()
	fleetLeader = leader.NewElector(stubLease{held: held}, "s", "me", time.Minute)
	if err := fleetLeader.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestChangeReviewReceiptsRunOnlyOnTheLeader(t *testing.T) {
	pool, mgr, reg, r := receiptsFixture(t)
	ctx := context.Background()
	prev := fleetLeader
	t.Cleanup(func() { fleetLeader = prev })
	job := receipt.NewJob(receipt.Deps{Logf: t.Logf})

	setLeader(t, true)
	if !runChangeReviewReceipts(ctx, job, mgr, reg) {
		t.Fatal("the leader did not run the receipts cycle")
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX cr_t_idx ON public.cr_t (a)`); err != nil {
		t.Fatal(err)
	}
	setLeader(t, false)
	if runChangeReviewReceipts(ctx, job, mgr, reg) {
		t.Fatal("a follower ran the receipts cycle")
	}
	got, ok := reg.Get("crdb")
	if !ok {
		t.Fatal("service unregistered")
	}
	if cur, _ := got.Get(ctx, r.ID); cur.LandingState != changereview.LandingAwaiting ||
		!cur.UpdatedAt.Equal(r.UpdatedAt) {
		t.Fatalf("a follower touched the review: landing %s, updated %s -> %s",
			cur.LandingState, r.UpdatedAt, cur.UpdatedAt)
	}
	setLeader(t, true)
	runChangeReviewReceipts(ctx, job, mgr, reg)
	if cur, _ := got.Get(ctx, r.ID); cur.LandingState != changereview.LandingLanded ||
		cur.LandingSource != changereview.LandingSourceCatalog {
		t.Fatalf("on the leader: landing %s/%s, want landed by catalog", cur.LandingState,
			cur.LandingSource)
	}
}

// Only databases with a registered Change Review service are covered.
func TestChangeReviewReceiptDatabases(t *testing.T) {
	_, mgr, reg, _ := receiptsFixture(t)
	dbs := changeReviewReceiptDatabases(mgr, reg)
	if len(dbs) != 1 || dbs[0].Name != "crdb" || dbs[0].Service == nil || dbs[0].Pool == nil {
		t.Fatalf("databases %+v, want crdb only", dbs)
	}
	if got := changeReviewReceiptDatabases(nil, reg); len(got) != 0 {
		t.Fatalf("a nil manager gave %+v", got)
	}
	if changeReviewServices() != changeReviewServices() {
		t.Fatal("the process-wide registry is not one registry")
	}
}
