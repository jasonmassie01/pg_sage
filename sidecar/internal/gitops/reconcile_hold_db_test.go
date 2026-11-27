package gitops

import (
	"context"
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// A held adopt leaves the API's set alone: when a file adopts an API set
// with a layout change that would widen a database, the adopt is held,
// and the file's narrower document must not be written onto the set the
// API still owns (cli-02a review).
func TestReconcile_HeldAdoptWritesNothingOnTheAPISet(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := policy.NewStore(f.pool).PutSet(ctx, policy.PutSetRequest{
		Set: policy.PolicySet{Name: "api-prod", Priority: 30,
			Selectors: []policy.Selector{{Tags: map[string]string{"tier": "prod"}}}},
		Actor: "carol@example.com", Dimensions: rubricDimensions()}); err != nil {
		t.Fatalf("API set: %v", err)
	}
	narrowAPISet(t, f, "api-prod")
	narrow := f.ceiling(t, "api-prod")
	// Adopting api-prod onto tier=stage moves orders back to a wider set.
	files := map[string]string{"policy-sets/api-prod.yaml": setFile("api-prod", 30,
		"stage", withCeiling(t, f.narrow, -1)) + "adopt: true\n"}
	before := f.rowCounts(t)
	res := f.apply(t, writeTree(t, files))
	if got := ops(res); got["policy_set:hold"] != 2 || len(got) != 1 || res.Applied {
		t.Fatalf("held adopt = %v applied=%v, want the adopt and its document held",
			got, res.Applied)
	}
	if got := f.ceiling(t, "api-prod"); got != narrow {
		t.Fatalf("api-prod ceiling = %v, want the API's %v untouched", got, narrow)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.policy_sets
		WHERE name = 'api-prod' AND managed_by = 'api'`); n != 1 {
		t.Fatal("the held adopt moved api-prod to config ownership")
	}
	if after := f.rowCounts(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("a held adopt wrote rows: %v -> %v", before, after)
	}
}

// narrowAPISet ratifies f.narrow on set as a person would.
func narrowAPISet(t *testing.T, f *fixture, set string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := policy.ProposeSetTx(ctx, tx, set, "unattended", f.narrow, "carol@example.com")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := policy.RatifyTx(ctx, tx, policy.RatifyRequest{ProposalID: p.ID,
		ExpectedVersion: p.BaseVersion, Actor: "carol@example.com"},
		p.CreatedAt); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
