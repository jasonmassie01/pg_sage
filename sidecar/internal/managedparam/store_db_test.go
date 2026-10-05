package managedparam

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

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/managedparam"))
}

var (
	poolOnce sync.Once
	sharedDB *pgxpool.Pool
	poolErr  error
)

func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	poolOnce.Do(func() {
		ctx := context.Background()
		sharedDB, poolErr = pgxpool.New(ctx, dsn)
		if poolErr == nil {
			poolErr = schema.Bootstrap(ctx, sharedDB)
		}
	})
	if poolErr != nil {
		t.Fatalf("test database: %v", poolErr)
	}
	ctx := context.Background()
	clean := func() {
		for _, stmt := range []string{"DELETE FROM sage.managed_change_proposals",
			"DELETE FROM sage.findings WHERE category LIKE 'mp_test%'"} {
			if _, err := sharedDB.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	return sharedDB, ctx
}

func buildProposal(t *testing.T, parameter, value string) Proposal {
	t.Helper()
	p, err := Build(intent(t, "rds", parameter, value), rdsTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStoreUpsertDedupesAndSupersedes(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	first, created, err := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 11)
	if err != nil || !created || first.Status != StatusPending || first.ID == 0 ||
		first.FindingID == nil || *first.FindingID != 11 {
		t.Fatalf("first upsert = %+v, %t, %v", first, created, err)
	}
	again, created, err := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 11)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("same fingerprint must refresh, not insert: %+v %t %v", again, created, err)
	}
	next, created, err := s.Upsert(ctx, buildProposal(t, "work_mem", "128MB"), 12)
	if err != nil || !created || next.ID == first.ID {
		t.Fatalf("new value = %+v %t %v", next, created, err)
	}
	old, err := s.Get(ctx, first.ID)
	if err != nil || old.Status != StatusSuperseded {
		t.Fatalf("older proposal for the same parameter = %+v, %v; want superseded", old, err)
	}
	other, created, err := s.Upsert(ctx, buildProposal(t, "shared_buffers", "4GB"), 13)
	if err != nil || !created {
		t.Fatalf("another parameter = %+v %t %v", other, created, err)
	}
	open, err := s.List(ctx, []string{StatusPending, StatusApproved}, 50)
	if err != nil || len(open) != 2 {
		t.Fatalf("open proposals = %d, %v", len(open), err)
	}
	if open[0].Proposal.Parameter == "" || open[0].Proposal.CLI == "" {
		t.Fatalf("the typed proposal must round-trip: %+v", open[0].Proposal)
	}
}

func TestStoreDecide(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rec, _, err := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.FindingID != nil {
		t.Fatalf("finding id 0 must store NULL, got %v", *rec.FindingID)
	}
	approved, err := s.Decide(ctx, rec.ID, true, 7, "will apply tonight")
	if err != nil || approved.Status != StatusApproved || approved.DecidedBy == nil ||
		*approved.DecidedBy != 7 || approved.DecisionNote != "will apply tonight" ||
		approved.DecidedAt == nil {
		t.Fatalf("approve = %+v, %v", approved, err)
	}
	if _, err := s.Decide(ctx, rec.ID, false, 8, ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second decision err = %v, want ErrNotPending", err)
	}
	if _, err := s.Decide(ctx, 999999, true, 7, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
	if _, err := s.Decide(ctx, rec.ID, true, 0, ""); err == nil {
		t.Fatal("a decision needs a user")
	}
	if _, err := s.Get(ctx, 424242); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown err = %v", err)
	}
}

// A rejected proposal is not proposed again for the cooldown.
func TestStoreRejectedIsNotReproposed(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rec, _, _ := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 1)
	if _, err := s.Decide(ctx, rec.ID, false, 3, "no"); err != nil {
		t.Fatal(err)
	}
	again, created, err := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 1)
	if err != nil || created || again.Status != StatusRejected {
		t.Fatalf("re-proposal after rejection = %+v %t %v", again, created, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.managed_change_proposals
		SET decided_at = now() - interval '8 days' WHERE id = $1`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 1); err != nil ||
		!created {
		t.Fatalf("after the cooldown the proposal returns: %t %v", created, err)
	}
}

// Two operators deciding at once: exactly one decision wins.
func TestStoreConcurrentDecisions(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rec, _, _ := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 1)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			_, err := s.Decide(ctx, rec.ID, user%2 == 0, user+1, "")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrNotPending) {
			t.Fatalf("unexpected error %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d decisions won, want exactly 1", wins)
	}
}

func TestStoreSupersedeAndApplied(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	keep, _, _ := s.Upsert(ctx, buildProposal(t, "work_mem", "64MB"), 1)
	gone, _, _ := s.Upsert(ctx, buildProposal(t, "shared_buffers", "4GB"), 2)
	n, err := s.SupersedeExcept(ctx, map[string]bool{keep.Proposal.Fingerprint: true})
	if err != nil || n != 1 {
		t.Fatalf("SupersedeExcept = %d, %v", n, err)
	}
	if rec, _ := s.Get(ctx, gone.ID); rec.Status != StatusSuperseded {
		t.Fatalf("gone = %+v", rec)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkApplied(ctx, keep.ID, at); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.Get(ctx, keep.ID)
	if rec.Status != StatusApplied || rec.AppliedAt == nil || !rec.AppliedAt.Equal(at) {
		t.Fatalf("applied = %+v", rec)
	}
	if err := s.MarkApplied(ctx, gone.ID, at); !errors.Is(err, ErrNotPending) {
		t.Fatalf("superseded cannot become applied: %v", err)
	}
	if _, err := s.List(ctx, []string{"bogus"}, 10); err == nil {
		t.Fatal("unknown status filter accepted")
	}
}
