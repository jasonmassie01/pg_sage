package approvalcard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/earned"
)

// The loader reads the database's Trust view once per request (a card, or
// the whole pending list), never once per card, and puts each card's
// pair on it.

type fakeTrust struct {
	mu    sync.Mutex
	calls int
	view  earned.TrustView
	bound bool
	err   error
}

func (f *fakeTrust) TrustView(_ context.Context, database string) (earned.TrustView, bool,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	v := f.view
	v.Database = database
	return v, f.bound, f.err
}

func indexTrustView() earned.TrustView {
	l1 := earned.L1
	return earned.TrustView{Rows: []earned.TrustRow{{Family: earned.FamilyTuning,
		Kind: earned.KindSelfInitiated, Class: earned.ClassIndexCreate, Level: earned.L1,
		Effective: &l1, Cap: earned.L3, Evidence: earned.TrustCounts{Improved: 1},
		Next: &earned.Assessment{Target: earned.L2, Checks: []earned.Check{{
			Name: "class_successes", Observed: "1", Required: "3",
			How: "2 more verified successes"}}}}}}
}

func TestLoaderPendingReadsTheTrustViewOnce(t *testing.T) {
	pool, ctx := livePool(t)
	a, _ := queueOptimizerIndex(t, ctx, pool)
	b, _ := queueOptimizerIndex(t, ctx, pool)
	src := &fakeTrust{view: indexTrustView(), bound: true}
	cards, err := Loader{Pool: pool, Database: "orders", Trust: src}.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if src.calls != 1 {
		t.Fatalf("trust view read %d times for %d cards, want once", src.calls, len(cards))
	}
	seen := 0
	for _, c := range cards {
		if c.QueueID != a && c.QueueID != b {
			continue
		}
		seen++
		if c.Trust == nil || c.Trust.Class != "index_create" || c.Trust.Level != "L1" ||
			c.Trust.NextLevel != "L2" || !strings.Contains(c.Trust.Line,
			"2 more verified successes") {
			t.Fatalf("card %d trust = %+v", c.QueueID, c.Trust)
		}
	}
	if seen != 2 {
		t.Fatalf("pending cards lack %d or %d", a, b)
	}
}

func TestLoaderCardTrustSourceStates(t *testing.T) {
	pool, ctx := livePool(t)
	id, _ := queueOptimizerIndex(t, ctx, pool)
	unbound := &fakeTrust{view: indexTrustView()}
	c, err := Loader{Pool: pool, Database: "orders", Trust: unbound}.Card(ctx, id)
	if err != nil || c.Trust != nil || unbound.calls != 1 {
		t.Fatalf("unbound database: trust %+v err %v calls %d", c.Trust, err, unbound.calls)
	}
	failing := &fakeTrust{bound: true, err: errors.New("ledger unreachable")}
	c, err = Loader{Pool: pool, Database: "orders", Trust: failing}.Card(ctx, id)
	if err != nil || c.Trust == nil || !strings.Contains(c.Trust.Unavailable,
		"ledger unreachable") {
		t.Fatalf("failing source: card trust %+v err %v", c.Trust, err)
	}
	c, err = Loader{Pool: pool, Database: "orders"}.Card(ctx, id)
	if err != nil || c.Trust != nil {
		t.Fatalf("no source: trust %+v err %v", c.Trust, err)
	}
}
