package drill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// drill-01a unit tests: the parts of a restore drill that need no
// database (tolerances, budgets, the fail-closed preconditions).
// No concurrent access tests here: these are pure functions and value
// types; the scheduler's concurrency is in scheduler_test.go.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/drill"))
}

// fakeAdapter is a clone adapter that hands out one fixed DSN.
type fakeAdapter struct {
	mu         sync.Mutex
	dsn        string
	from       time.Time
	kind       clone.Kind
	createErr  error
	destroyErr error
	n          int
	created    []string
	destroyed  []string
}

func (f *fakeAdapter) Create(context.Context, clone.CloneSpec) (clone.Clone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return clone.Clone{}, f.createErr
	}
	f.n++
	id := fmt.Sprintf("drill-clone-%d", f.n)
	f.created = append(f.created, id)
	return clone.Clone{ID: id, DSN: f.dsn, CreatedFrom: f.from}, nil
}

func (f *fakeAdapter) Destroy(_ context.Context, c clone.Clone) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, c.ID)
	return f.destroyErr
}

func (f *fakeAdapter) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

func (f *fakeAdapter) Describe(context.Context) (clone.Capability, error) {
	kind := f.kind
	if kind == "" {
		kind = clone.KindBranch
	}
	return clone.Capability{Kind: kind}, nil
}

func (f *fakeAdapter) counts() (created, destroyed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.destroyed)
}

func TestWithinTolerance_Boundary(t *testing.T) {
	cases := []struct {
		source, clone float64
		want          bool
	}{
		{1000, 1000, true},
		{1000, 950, true},  // exactly 5.0% low
		{1000, 1050, true}, // exactly 5.0% high
		{1000, 949, false}, // 5.1% low
		{1000, 1051, false},
		{0, 0, true},
		{0, 1, false},
		{100, 90, false}, // the corrupted backup: 10% missing
	}
	for _, c := range cases {
		if got := WithinTolerance(c.source, c.clone); got != c.want {
			t.Errorf("WithinTolerance(%v, %v) = %v, want %v", c.source, c.clone, got, c.want)
		}
	}
}

func TestReceiptTTL_IsTheBudgetPlusAnHour(t *testing.T) {
	if got := ReceiptTTL(Budget{MaxDuration: 30 * time.Minute}); got != 90*time.Minute {
		t.Fatalf("TTL = %s, want 1h30m", got)
	}
	if got := ReceiptTTL(Budget{}); got != DefaultMaxDuration+time.Hour {
		t.Fatalf("zero budget TTL = %s, want the default budget plus an hour", got)
	}
	if got := ReceiptTTL(Budget{MaxDuration: -time.Minute}); got != DefaultMaxDuration+time.Hour {
		t.Fatalf("negative budget TTL = %s, want the default", got)
	}
}

func TestRun_NoProviderIsRecoverabilityUnknown(t *testing.T) {
	res, err := Drill{Database: "orders", Store: NewStore(nil)}.Run(context.Background())
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("err = %v, want ErrNoProvider", err)
	}
	if res.Passed {
		t.Fatal("a drill without a provider passed")
	}
}

func TestRun_RefusesAProviderWithoutReceipts(t *testing.T) {
	adapter := &fakeAdapter{dsn: "postgres://clone.invalid/app"}
	_, err := Drill{Database: "orders", Provider: adapter, Store: NewStore(nil)}.
		Run(context.Background())
	if !errors.Is(err, ErrNotReceipted) {
		t.Fatalf("err = %v, want ErrNotReceipted", err)
	}
	if created, _ := adapter.counts(); created != 0 {
		t.Fatalf("an unreceipted provider was asked for %d clones", created)
	}
}

func TestRun_NeedsADatabaseAndAStore(t *testing.T) {
	p, err := NewReceiptedProvider(&fakeAdapter{}, clone.NewReceiptStore(nil),
		ProviderConfig{Adapter: "dle", Scope: "unit", Deployment: func(context.Context) (
			string, error) {
			return "", errors.New("unused")
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Drill{Provider: p, Store: NewStore(nil)}).Run(context.Background()); err == nil {
		t.Fatal("a drill without a database name ran")
	}
	if _, err := (Drill{Database: "orders", Provider: p}).Run(context.Background()); err == nil {
		t.Fatal("a drill without a result store ran")
	}
}

func TestNewReceiptedProvider_WritesDrillReceipts(t *testing.T) {
	if _, err := NewReceiptedProvider(nil, nil, ProviderConfig{}); err == nil {
		t.Fatal("nil adapter accepted")
	}
	_, err := NewReceiptedProvider(&fakeAdapter{}, nil, ProviderConfig{Adapter: "dle"})
	if err == nil {
		t.Fatal("missing scope and deployment accepted")
	}
}

func TestRPO_IsTheRecoveryPointMinusTheMarker(t *testing.T) {
	rp := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	if got, ok := rpoSeconds(rp, rp.Add(-90*time.Second)); !ok || got != 90 {
		t.Fatalf("rpo = %v %v, want 90", got, ok)
	}
	if _, ok := rpoSeconds(rp, time.Time{}); ok {
		t.Fatal("an unknown marker gave an RPO")
	}
	if got, ok := rpoSeconds(rp, rp.Add(time.Minute)); !ok || got != 0 {
		t.Fatalf("a marker after the recovery point = %v %v, want 0", got, ok)
	}
}

func TestPassed_AnyFailingInvariantFails(t *testing.T) {
	ok := []Invariant{{Name: "catalog", Status: StatusPass},
		{Name: "sample_checksum", Status: StatusUnknown}}
	if !allPassed(ok) {
		t.Fatal("pass and unknown invariants must pass the drill")
	}
	if allPassed(append(ok, Invariant{Name: "row_count", Status: StatusFail})) {
		t.Fatal("a failing invariant passed the drill")
	}
	if !allPassed(nil) {
		t.Fatal("no invariants (no tables) must not fail the drill")
	}
}
