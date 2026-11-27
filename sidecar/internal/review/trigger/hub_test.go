package trigger

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// The hub without a database: what a source hook sees when no review
// queue is registered, and the error metric it bumps.

func TestHub_UnknownDatabaseIsAnErrorAndCounted(t *testing.T) {
	h := NewHub()
	before := FireErrors()[KindPreUpgrade]
	err := h.PreUpgrade(context.Background(), "nosuch", 18, "pg_upgrade", t0)
	if !errors.Is(err, ErrUnknownDatabase) || !strings.Contains(err.Error(), "nosuch") ||
		!strings.Contains(err.Error(), string(KindPreUpgrade)) {
		t.Fatalf("err = %v, want ErrUnknownDatabase naming the database and kind", err)
	}
	if got := FireErrors()[KindPreUpgrade]; got != before+1 {
		t.Fatalf("pre_upgrade fire errors = %d, want %d", got, before+1)
	}
}

func TestHub_EmptyNameResolvesOnlyASingleDatabase(t *testing.T) {
	h := NewHub()
	shop, crm := NewStore(nil), NewStore(nil)
	h.Register("shop", shop, 10<<30)
	if db, ok := h.resolve(""); !ok || db != "shop" {
		t.Fatalf("resolve(\"\") = %q %v, want the only database", db, ok)
	}
	h.Register("crm", crm, 10<<30)
	if db, ok := h.resolve(""); ok {
		t.Fatalf("resolve(\"\") = %q with two databases, want none", db)
	}
	h.Unregister("crm", crm)
	h.Unregister("shop", shop)
	if _, ok := h.resolve("shop"); ok {
		t.Fatal("an unregistered database still resolves")
	}
}

// A rebuilt runtime registers its queue before the replaced runtime stops:
// the old runtime's Unregister must leave the new queue in place.
func TestHub_StaleUnregisterKeepsTheReplacement(t *testing.T) {
	h := NewHub()
	old, repl := NewStore(nil), NewStore(nil)
	h.Register("shop", old, 1<<30)
	h.Register("shop", repl, 2<<30)
	h.Unregister("shop", old)
	_, e, err := h.entry("shop", KindPreUpgrade)
	if err != nil || e.store != repl || e.largeBytes != 2<<30 {
		t.Fatalf("after a stale unregister: entry %+v, %v; want the replacement", e, err)
	}
	h.Unregister("shop", repl)
	if _, ok := h.resolve("shop"); ok {
		t.Fatal("the current queue's unregister left it registered")
	}
}

// A registered database with a nil pool: the insert fails with ErrNoStore,
// returned to the hook and counted.
func TestHub_StoreWithoutAPoolFailsTheHook(t *testing.T) {
	h := NewHub()
	h.Register("shop", NewStore(nil), 1<<30)
	before := FireErrors()[KindLargeMigration]
	err := h.LargeMigration(context.Background(), "shop",
		[]TableSize{{Table: "public.big", Bytes: 2 << 30}}, t0)
	if !errors.Is(err, ErrNoStore) || !strings.Contains(err.Error(), "shop") {
		t.Fatalf("err = %v, want ErrNoStore naming shop", err)
	}
	if got := FireErrors()[KindLargeMigration]; got != before+1 {
		t.Fatalf("large_migration fire errors = %d, want %d", got, before+1)
	}
}

// A migration touching only small tables fires nothing and is not an
// error, even without a usable store.
func TestHub_SmallMigrationFiresNothing(t *testing.T) {
	h := NewHub()
	h.Register("shop", NewStore(nil), 10<<30)
	err := h.LargeMigration(context.Background(), "shop",
		[]TableSize{{Table: "public.small", Bytes: 10 << 30}}, t0)
	if err != nil {
		t.Fatalf("err = %v, want nil for a table at the threshold", err)
	}
}

func TestStore_FireRefusesInvalidInputBeforeTheDatabase(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()
	cases := []Trigger{
		{Database: "shop", Kind: "nightly", Key: "k"},
		{Database: "", Kind: KindCalendar, Key: "k"},
		{Database: "shop", Kind: KindCalendar, Key: ""},
		{Database: "shop", Kind: KindCalendar, Key: strings.Repeat("k", 301)},
	}
	for _, c := range cases {
		if _, _, err := s.Fire(ctx, c); !errors.Is(err, ErrInvalid) {
			t.Errorf("Fire(%+v) = %v, want ErrInvalid", c, err)
		}
	}
	var nilStore *Store
	if _, _, err := nilStore.Fire(ctx, Trigger{Database: "shop", Kind: KindCalendar,
		Key: "k"}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store: %v, want ErrNoStore", err)
	}
}

func TestFireErrors_CountsUnderConcurrency(t *testing.T) {
	before := FireErrors()[KindInstanceChange]
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			countFireError(KindInstanceChange)
		}()
	}
	wg.Wait()
	if got := FireErrors()[KindInstanceChange]; got != before+50 {
		t.Fatalf("instance_change fire errors = %d, want %d", got, before+50)
	}
}
