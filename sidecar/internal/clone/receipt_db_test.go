package clone

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Spec §6.12 and §6.5: one atomic receipt INSERT before the provider call;
// a ready, unexpired receipt names the provider resource a branch label is
// bound to.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/clone"))
}

func receiptStore(t *testing.T) (*ReceiptStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	clean := func() { _, _ = pool.Exec(context.Background(), "DELETE FROM sage.clone_instances") }
	clean()
	t.Cleanup(clean)
	return NewReceiptStore(pool), pool, ctx
}

const testDeployment = "00000000-0000-4000-8000-00000000c10e"

func validReceipt(name string) Receipt {
	return Receipt{DeploymentID: testDeployment, Adapter: "dle", Scope: "lab", Name: name,
		Purpose: PurposeSandbox, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestReceipt_RecordThenReady(t *testing.T) {
	s, _, ctx := receiptStore(t)
	r, err := s.Record(ctx, validReceipt("sbx-1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.Status != StatusCreating || r.Ref != "" || r.CreatedAt.IsZero() {
		t.Fatalf("recorded: %+v", r)
	}
	if _, ok, err := s.Active(ctx, "sbx-1"); err != nil || ok {
		t.Fatalf("a creating receipt is not active: %v %v", ok, err)
	}
	ready, err := s.MarkReady(ctx, r.ID, "clone-9")
	if err != nil || ready.Status != StatusReady || ready.Ref != "clone-9" {
		t.Fatalf("ready: %+v %v", ready, err)
	}
	got, ok, err := s.Active(ctx, "sbx-1")
	if err != nil || !ok || got.ID != r.ID || got.ProviderRef() != "dle:clone-9" {
		t.Fatalf("active: %+v %v %v", got, ok, err)
	}
	if _, err := s.MarkReady(ctx, r.ID, "clone-10"); !errors.Is(err, ErrReceiptState) {
		t.Fatalf("a ready receipt cannot be re-pointed: %v", err)
	}
}

func TestReceipt_Invalid(t *testing.T) {
	s, _, ctx := receiptStore(t)
	for name, mut := range map[string]func(*Receipt){
		"no deployment": func(r *Receipt) { r.DeploymentID = "" },
		"bad uuid":      func(r *Receipt) { r.DeploymentID = "nope" },
		"no adapter":    func(r *Receipt) { r.Adapter = "" },
		"no scope":      func(r *Receipt) { r.Scope = "" },
		"no name":       func(r *Receipt) { r.Name = "" },
		"bad purpose":   func(r *Receipt) { r.Purpose = "fun" },
		"expired":       func(r *Receipt) { r.ExpiresAt = time.Now().Add(-time.Minute) },
		"zero expiry":   func(r *Receipt) { r.ExpiresAt = time.Time{} },
		"long name":     func(r *Receipt) { r.Name = string(make([]byte, 300)) },
	} {
		r := validReceipt("x")
		mut(&r)
		if _, err := s.Record(ctx, r); !errors.Is(err, ErrInvalidReceipt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.MarkReady(ctx, 999999, "ref"); !errors.Is(err, ErrReceiptNotFound) {
		t.Errorf("unknown receipt: %v", err)
	}
	r, err := s.Record(ctx, validReceipt("sbx-empty"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkReady(ctx, r.ID, " "); !errors.Is(err, ErrInvalidReceipt) {
		t.Errorf("blank provider ref: %v", err)
	}
}

func TestReceipt_DuplicateName(t *testing.T) {
	s, _, ctx := receiptStore(t)
	if _, err := s.Record(ctx, validReceipt("dup")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, validReceipt("dup")); !errors.Is(err, ErrReceiptExists) {
		t.Fatalf("second receipt for (adapter, scope, name): %v", err)
	}
}

func TestReceipt_ActiveIgnoresExpiredAndPicksNewest(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	old := validReceipt("sbx-n")
	old.Scope = "a"
	r1, _ := s.Record(ctx, old)
	if _, err := s.MarkReady(ctx, r1.ID, "c1"); err != nil {
		t.Fatal(err)
	}
	newer := validReceipt("sbx-n")
	newer.Scope = "b"
	r2, _ := s.Record(ctx, newer)
	if _, err := s.MarkReady(ctx, r2.ID, "c2"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Active(ctx, "sbx-n")
	if err != nil || !ok || got.ID != r2.ID {
		t.Fatalf("newest: %+v %v %v", got, ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances
		SET expires_at = now() - interval '1 second' WHERE id = $1`, r2.ID); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.Active(ctx, "sbx-n")
	if err != nil || !ok || got.ID != r1.ID {
		t.Fatalf("expired newest skipped: %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.Active(ctx, ""); err != nil || ok {
		t.Fatalf("blank name: %v %v", ok, err)
	}
}

func TestReceipt_ConcurrentMarkReady(t *testing.T) {
	s, _, ctx := receiptStore(t)
	r, err := s.Record(ctx, validReceipt("race"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.MarkReady(ctx, r.ID, "ref"); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrReceiptState) {
				t.Errorf("racer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d racers marked the receipt ready, want exactly 1", wins)
	}
}

func TestReceipt_NilPool(t *testing.T) {
	s := NewReceiptStore(nil)
	if _, err := s.Record(context.Background(), validReceipt("x")); !errors.Is(err,
		ErrNoReceiptStore) {
		t.Fatalf("nil pool record: %v", err)
	}
	if _, _, err := s.Active(context.Background(), "x"); !errors.Is(err, ErrNoReceiptStore) {
		t.Fatalf("nil pool active: %v", err)
	}
}
