package fleetlearn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

func lease(t *testing.T, pool *pgxpool.Pool, scope, holder string, epoch int64,
	ttl time.Duration) {
	t.Helper()
	exec(t, pool, `INSERT INTO sage.fleet_leader_lease (scope, holder, epoch,
		acquired_at, renewed_at, expires_at)
		VALUES ($1,$2,$3,now(),now(),now() + $4 * interval '1 millisecond')
		ON CONFLICT (scope) DO UPDATE SET holder=EXCLUDED.holder,
		epoch=EXCLUDED.epoch, expires_at=EXCLUDED.expires_at`,
		scope, holder, epoch, ttl.Milliseconds())
}

func TestStore_SaveAndLoadFingerprintsIsolatedByScope(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "store_fp")
	s1 := NewStore(pool, "scope-1")
	s2 := NewStore(pool, "scope-2")
	a := Fingerprint{Database: "a", Boundary: "tenant:x", Tables: []string{"t1", "t2"},
		Indexes: []string{"i1"}, Queries: []string{"q1"}, ComputedAt: time.Now()}
	if err := s1.SaveFingerprint(ctx, Fence{}, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s2.SaveFingerprint(ctx, Fence{}, Fingerprint{Database: "z",
		Tables: []string{"t9"}}); err != nil {
		t.Fatalf("save other scope: %v", err)
	}
	got, err := s1.Fingerprints(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Database != "a" || got[0].Boundary != "tenant:x" ||
		len(got[0].Tables) != 2 || got[0].Queries[0] != "q1" {
		t.Fatalf("scope-1 fingerprints = %+v", got)
	}
	a.Tables = []string{"t3"}
	if err := s1.SaveFingerprint(ctx, Fence{}, a); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = s1.Fingerprints(ctx)
	if len(got) != 1 || len(got[0].Tables) != 1 || got[0].Tables[0] != "t3" {
		t.Fatalf("overwrite not applied: %+v", got)
	}
}

func TestStore_LabelsRoundTripOnlyWhenPresent(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "store_labels")
	s := NewStore(pool, "scope")
	err := s.SaveFingerprint(ctx, Fence{}, Fingerprint{Database: "a",
		Tables: []string{"h"}, Labels: map[string]string{"h": "public.orders"}})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SaveFingerprint(ctx, Fence{}, Fingerprint{Database: "b",
		Tables: []string{"h"}}); err != nil {
		t.Fatalf("save b: %v", err)
	}
	got, _ := s.Fingerprints(ctx)
	byDB := map[string]Fingerprint{}
	for _, f := range got {
		byDB[f.Database] = f
	}
	if byDB["a"].Labels["h"] != "public.orders" {
		t.Fatalf("labels lost: %+v", byDB["a"])
	}
	if byDB["b"].Labels != nil {
		t.Fatalf("labels invented: %+v", byDB["b"])
	}
}

func TestStore_DigestsReplaceAndFilter(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "store_digest")
	s := NewStore(pool, "scope")
	first := []OutcomeCount{{Class: "index_create", Shape: "S", Improved: 1},
		{Class: "guc", Shape: "", Regressed: 2}}
	if err := s.SaveDigest(ctx, Fence{}, "a", first); err != nil {
		t.Fatalf("save: %v", err)
	}
	second := []OutcomeCount{{Class: "index_create", Shape: "S", Improved: 5}}
	if err := s.SaveDigest(ctx, Fence{}, "a", second); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := s.SaveDigest(ctx, Fence{}, "b", first); err != nil {
		t.Fatalf("save b: %v", err)
	}
	got, err := s.Digests(ctx, []string{"a"}, "index_create")
	if err != nil {
		t.Fatalf("digests: %v", err)
	}
	if len(got) != 1 || len(got["a"]) != 1 || got["a"][0].Improved != 5 {
		t.Fatalf("digests = %+v, want a's replaced index_create row only", got)
	}
	guc, _ := s.Digests(ctx, []string{"a", "b"}, "guc")
	if len(guc["a"]) != 0 || len(guc["b"]) != 1 {
		t.Fatalf("a's guc row must be replaced away; got %+v", guc)
	}
	none, err := s.Digests(ctx, nil, "guc")
	if err != nil || len(none) != 0 {
		t.Fatalf("no databases = %+v,%v", none, err)
	}
}

func TestStore_PruneRemovesDepartedDatabases(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "store_prune")
	s := NewStore(pool, "scope")
	for _, db := range []string{"a", "b", "c"} {
		if err := s.SaveFingerprint(ctx, Fence{}, Fingerprint{Database: db,
			Tables: []string{"t"}}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := s.SaveDigest(ctx, Fence{}, db, []OutcomeCount{{Class: "guc"}}); err != nil {
			t.Fatalf("digest: %v", err)
		}
	}
	if err := s.Prune(ctx, Fence{}, []string{"a", "c"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, _ := s.Fingerprints(ctx)
	if len(got) != 2 {
		t.Fatalf("after prune = %+v, want a and c", got)
	}
	d, _ := s.Digests(ctx, []string{"b"}, "guc")
	if len(d["b"]) != 0 {
		t.Fatalf("b's digest survived the prune: %+v", d)
	}
	if err := s.Prune(ctx, Fence{}, nil); err != nil {
		t.Fatalf("prune all: %v", err)
	}
	if got, _ := s.Fingerprints(ctx); len(got) != 0 {
		t.Fatalf("prune with no keepers left %+v", got)
	}
}

func TestStore_FencedWritesRequireTheCurrentLease(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "store_fence")
	s := NewStore(pool, "scope")
	fp := Fingerprint{Database: "a", Tables: []string{"t"}}
	fence := Fence{Holder: "me", Epoch: 2}

	if err := s.SaveFingerprint(ctx, fence, fp); !errors.Is(err, ErrFenced) {
		t.Fatalf("no lease: err = %v, want ErrFenced", err)
	}
	lease(t, pool, "scope", "me", 2, time.Minute)
	if err := s.SaveFingerprint(ctx, fence, fp); err != nil {
		t.Fatalf("current lease: %v", err)
	}
	lease(t, pool, "scope", "other", 3, time.Minute)
	if err := s.SaveDigest(ctx, fence, "a", nil); !errors.Is(err, ErrFenced) {
		t.Fatalf("taken over: err = %v, want ErrFenced", err)
	}
	if err := s.Prune(ctx, fence, nil); !errors.Is(err, ErrFenced) {
		t.Fatalf("taken over prune: err = %v, want ErrFenced", err)
	}
	lease(t, pool, "scope", "me", 2, -time.Second)
	if err := s.SaveFingerprint(ctx, fence, fp); !errors.Is(err, ErrFenced) {
		t.Fatalf("expired lease: err = %v, want ErrFenced", err)
	}
	if got, _ := s.Fingerprints(ctx); len(got) != 1 {
		t.Fatalf("a fenced-out write changed state: %+v", got)
	}
	lease(t, pool, "other-scope", "me", 2, time.Minute)
	if err := s.SaveFingerprint(ctx, fence, fp); !errors.Is(err, ErrFenced) {
		t.Fatalf("lease of another scope must not fence this one: %v", err)
	}
}

func TestStore_ConcurrentDigestWritesStayConsistent(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "store_conc")
	s := NewStore(pool, "scope")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs <- s.SaveDigest(ctx, Fence{}, "a",
				[]OutcomeCount{{Class: "guc", Improved: n}})
		}(i + 1)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent save: %v", err)
		}
	}
	got, _ := s.Digests(ctx, []string{"a"}, "guc")
	if len(got["a"]) != 1 {
		t.Fatalf("concurrent replaces must leave exactly one row, got %+v", got)
	}
}

func TestStore_NilPoolErrors(t *testing.T) {
	s := NewStore(nil, "scope")
	if err := s.SaveFingerprint(context.Background(), Fence{}, Fingerprint{}); err == nil {
		t.Fatal("nil pool save must fail")
	}
	if _, err := s.Fingerprints(context.Background()); err == nil {
		t.Fatal("nil pool load must fail")
	}
}

func TestMigrationIsIdempotentAndCreatesTheTables(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "store_migrate")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	for _, table := range []string{"sage.fleet_fingerprint", "sage.fleet_outcome_digest",
		"sage.fleet_leader_lease"} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL",
			table).Scan(&exists); err != nil || !exists {
			t.Fatalf("%s exists=%v err=%v", table, exists, err)
		}
	}
}
