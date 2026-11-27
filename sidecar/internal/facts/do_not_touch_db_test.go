package facts

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// evid-03b on real PostgreSQL: a declared do_not_touch fact binds through
// the store and the catalog resolver (an index resolves to its table),
// expiring it (recorded with who and why) lifts it, and a new declaration
// binds again. No concurrent access tests here: the snapshot read is
// covered in do_not_touch_test.go and the store's cache in
// store_cache_db_test.go.

func doNotTouchTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	table, index := uniqueName("orders_dnt_"), uniqueName("idx_dnt_")
	execAll(t, ctx, pool, "CREATE TABLE public."+table+" (id int, status text)",
		"CREATE INDEX "+index+" ON public."+table+" (status)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	return table, index
}

func liveBind(t *testing.T, ctx context.Context, b *Binder, action, sql string,
	operator, rollback bool) []policy.FactBinding {
	t.Helper()
	got, err := b.Bind(ctx, policy.ActionRequest{SQL: sql, OperatorApproved: operator,
		Rollback: rollback, Contract: &policy.ActionContract{ActionType: action}})
	if err != nil {
		t.Fatalf("bind %q: %v", sql, err)
	}
	return got
}

func TestDoNotTouchLifecycleOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	table, index := doNotTouchTable(t, ctx, pool)
	s := NewStore(pool)
	b := NewBinder(s, NewCatalogResolver(pool), nil)
	p := Proposal{Type: TypeDoNotTouch, Kind: KindTable, Subject: "public." + table,
		Value: map[string]string{"note": "payments ledger"}}
	f, err := s.Declare(ctx, p, "alice@example.com", "owner brief")
	if err != nil || f.Status != StatusConfirmed || f.Type != TypeDoNotTouch {
		t.Fatalf("declare: %+v %v", f, err)
	}
	reindex := "REINDEX INDEX CONCURRENTLY public." + index
	for name, sql := range map[string]string{
		"index create": "CREATE INDEX CONCURRENTLY x ON public." + table + " (id)",
		"vacuum":       "VACUUM public." + table,
		"reindex":      reindex,
	} {
		got := liveBind(t, ctx, b, "reindex_index", sql, true, false)
		if len(got) != 1 || got[0].FactID != f.ID || got[0].Route != "hold" ||
			got[0].ConfirmedBy != "alice@example.com" {
			t.Fatalf("%s: bindings %+v, want fact %d held", name, got, f.ID)
		}
	}
	if got := liveBind(t, ctx, b, "revert_created_index",
		"DROP INDEX CONCURRENTLY public."+index, true, true); got != nil {
		t.Fatalf("the rollback of pg_sage's own index was bound: %+v", got)
	}
	expired, err := s.Expire(ctx, f.ID, "bob@example.com", "migration window agreed")
	if err != nil || expired.Status != StatusExpired || expired.ExpiredBy != "bob@example.com" ||
		expired.ExpiredReason != "migration window agreed" {
		t.Fatalf("expire: %+v %v", expired, err)
	}
	if got := liveBind(t, ctx, b, "reindex_index", reindex, true, false); got != nil {
		t.Fatalf("an expired fact still binds: %+v", got)
	}
	again, err := s.Declare(ctx, p, "carol@example.com", "")
	if err != nil || again.ID != f.ID || again.Status != StatusConfirmed {
		t.Fatalf("declare again: %+v %v", again, err)
	}
	got := liveBind(t, ctx, b, "reindex_index", reindex, false, false)
	if len(got) != 1 || got[0].ConfirmedBy != "carol@example.com" {
		t.Fatalf("redeclared fact bindings %+v", got)
	}
}

// A slot or a pattern that could match pg_sage's schema is refused by the
// store; nothing is stored.
func TestDoNotTouchRefusedSubjectsOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	for _, p := range []Proposal{
		{Type: TypeDoNotTouch, Kind: KindSlot, Subject: "cdc_orders"},
		{Type: TypeDoNotTouch, Kind: KindTable, Subject: "*.*"},
	} {
		if _, err := s.Declare(ctx, p, "alice@example.com", ""); err == nil {
			t.Fatalf("declared %+v", p)
		}
	}
	list, err := s.List(ctx, Filter{Type: TypeDoNotTouch})
	if err != nil || len(list) != 0 {
		t.Fatalf("stored after refusals: %+v %v", list, err)
	}
}

// An unreadable fact store is an error, never "nothing binds".
func TestDoNotTouchUnreadableStoreFailsOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	b := NewBinder(NewStore(other), nil, nil)
	_, err = b.Bind(ctx, policy.ActionRequest{SQL: "VACUUM public.orders",
		Contract: &policy.ActionContract{ActionType: "vacuum_table"}})
	if err == nil {
		t.Fatal("a closed pool bound nothing without an error")
	}
}
