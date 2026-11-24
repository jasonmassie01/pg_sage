package histstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// useProcessMode sets the process mode for one test and restores the
// previous one after it.
func useProcessMode(t *testing.T, m Mode) {
	t.Helper()
	prev := ProcessMode()
	if err := SetProcessMode(m); err != nil {
		t.Fatalf("SetProcessMode(%q): %v", m, err)
	}
	t.Cleanup(func() {
		if err := SetProcessMode(prev); err != nil {
			t.Errorf("restore process mode %q: %v", prev, err)
		}
	})
}

// refusesAll asserts every statement on s fails with ErrNotRegistered and
// that none reached db.
func refusesAll(t *testing.T, s Store, db *fakeDB) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Exec(ctx, "SELECT 1"); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Exec: want ErrNotRegistered, got %v", err)
	}
	if _, err := s.Query(ctx, "SELECT 1"); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Query: want ErrNotRegistered, got %v", err)
	}
	if err := s.QueryRow(ctx, "SELECT 1").Scan(new(int)); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("QueryRow: want ErrNotRegistered, got %v", err)
	}
	if _, err := s.Begin(ctx); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Begin: want ErrNotRegistered, got %v", err)
	}
	if db != nil {
		db.mu.Lock()
		n := len(db.sql)
		db.mu.Unlock()
		if n != 0 {
			t.Fatalf("a refused store ran %d statements on the monitored database", n)
		}
	}
}

func TestProcessModeDefaultsToMonitored(t *testing.T) {
	if got := ProcessMode(); got != ModeMonitored {
		t.Fatalf("default process mode = %q, want %q", got, ModeMonitored)
	}
}

func TestSetProcessModeRejectsUnknownModes(t *testing.T) {
	useProcessMode(t, ModeMeta)
	for _, m := range []Mode{"", "Meta", "bogus"} {
		err := SetProcessMode(m)
		if err == nil || !strings.Contains(err.Error(), string(ModeMeta)) {
			t.Fatalf("SetProcessMode(%q) = %v, want an error naming the valid modes", m, err)
		}
		if got := ProcessMode(); got != ModeMeta {
			t.Fatalf("a refused mode changed the process mode to %q", got)
		}
	}
}

// State transitions: in meta mode an unregistered pool is refused,
// registering it makes it work and unregistering refuses it again.
func TestResolveMetaModeRefusesUnregisteredPool(t *testing.T) {
	useProcessMode(t, ModeMeta)
	monitored, meta := &fakeDB{}, &fakeDB{}
	refusesAll(t, Resolve(monitored), monitored)

	unreg := Register(monitored, "app", mustMeta(t, meta, 11))
	s := Resolve(monitored)
	if !s.Scoped() || s.DatabaseID() != 11 {
		t.Fatalf("registered pool resolves to %+v, want the meta store 11", s)
	}
	if _, err := s.Exec(context.Background(), "SELECT 1 WHERE {db:}"); err != nil {
		t.Fatalf("registered Exec: %v", err)
	}
	meta.mu.Lock()
	got := append([]string(nil), meta.sql...)
	meta.mu.Unlock()
	if len(got) != 1 || got[0] != "SELECT 1 WHERE database_id = $1" {
		t.Fatalf("meta database statements = %q", got)
	}

	unreg()
	refusesAll(t, Resolve(monitored), monitored)
}

func TestRefusedStoreNamesThePoolAndTheFix(t *testing.T) {
	useProcessMode(t, ModeMeta)
	_, err := Resolve(&fakeDB{}).Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("unregistered pool in meta mode ran a statement")
	}
	msg := err.Error()
	for _, want := range []string{"*histstore.fakeDB", "history.store", "meta", "Register"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

// A refused store stays refused on another handle: WithDB on a
// transaction cannot reopen the monitored database.
func TestRefusedStoreWithDBStaysRefused(t *testing.T) {
	useProcessMode(t, ModeMeta)
	other := &fakeDB{}
	refusesAll(t, Resolve(&fakeDB{}).WithDB(other), other)
}

func TestRefusedCountsEveryRefusedStatement(t *testing.T) {
	useProcessMode(t, ModeMeta)
	before := Refused()
	refusesAll(t, Resolve(&fakeDB{}), nil)
	if got := Refused() - before; got != 4 {
		t.Fatalf("refused statements counted = %d, want 4", got)
	}
	useProcessMode(t, ModeMonitored)
	before = Refused()
	if _, err := Resolve(&fakeDB{}).Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("monitored fallback Exec: %v", err)
	}
	if got := Refused() - before; got != 0 {
		t.Fatalf("monitored mode counted %d refusals, want 0", got)
	}
}

// Unchanged in meta mode: nil, a Store passed directly, and a registered
// pool. A handle that can never be registered (not hashable) is refused.
func TestResolveMetaModeKeepsNilAndStores(t *testing.T) {
	useProcessMode(t, ModeMeta)
	var typedNil *pgxpool.Pool
	for name, db := range map[string]any{"nil": nil, "typed nil": typedNil} {
		if got := Resolve(db); got != (Store{}) {
			t.Fatalf("Resolve(%s) = %+v, want Store{}", name, got)
		}
	}
	direct := mustMeta(t, &fakeDB{}, 5)
	if got := Resolve(direct); got != direct {
		t.Fatalf("a Store passed directly resolved to %+v", got)
	}
	mon := NewMonitored(&fakeDB{})
	if _, err := Resolve(mon).Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("a monitored Store passed directly must still run in meta mode: %v", err)
	}
	type unhashable struct{ _ []string } // a slice field makes it unhashable
	refusesAll(t, Resolve(unhashable{}), nil)
}

func TestResolveNilIsZeroStoreInBothModes(t *testing.T) {
	for _, m := range []Mode{ModeMonitored, ModeMeta} {
		useProcessMode(t, m)
		if got := Resolve(nil); got != (Store{}) {
			t.Fatalf("%s: Resolve(nil) = %+v, want Store{}", m, got)
		}
		if _, err := Resolve(nil).Exec(context.Background(), "SELECT 1"); !errors.Is(err,
			ErrNoDatabase) {
			t.Fatalf("%s: Resolve(nil).Exec: want ErrNoDatabase, got %v", m, err)
		}
	}
}

// Switching back to monitored mode restores the fallback to the pool.
func TestResolveMetaToMonitoredRestoresFallback(t *testing.T) {
	db := &fakeDB{}
	useProcessMode(t, ModeMeta)
	refusesAll(t, Resolve(db), db)
	if err := SetProcessMode(ModeMonitored); err != nil {
		t.Fatal(err)
	}
	s := Resolve(db)
	if s.Scoped() || s.DB() != db {
		t.Fatalf("monitored mode must resolve an unregistered pool to itself: %+v", s)
	}
	if _, err := s.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("monitored fallback Exec: %v", err)
	}
}

// Concurrency: Resolve runs while the pool is registered and unregistered.
// Every statement either reaches the meta store or is refused; none falls
// back to the monitored database.
func TestResolveConcurrentWithRegisterInMetaMode(t *testing.T) {
	useProcessMode(t, ModeMeta)
	monitored, meta := &fakeDB{}, &fakeDB{}
	store := mustMeta(t, meta, 3)
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				Register(monitored, "app", store)()
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, err := Resolve(monitored).Exec(context.Background(), "SELECT 1")
				if err != nil && !errors.Is(err, ErrNotRegistered) {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent resolve: unexpected error %v", err)
	}
	monitored.mu.Lock()
	n := len(monitored.sql)
	monitored.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d statements fell back to the monitored database in meta mode", n)
	}
}

// Err is how code that binds statements itself (Bind) sees a refusal:
// ErrNotRegistered, counted, for a refused store; nil for a usable store
// and for the zero Store (whose statements fail with ErrNoDatabase).
func TestStoreErrReportsOnlyRefusals(t *testing.T) {
	useProcessMode(t, ModeMeta)
	refusedStore := Resolve(&fakeDB{})
	before := Refused()
	if err := refusedStore.Err(); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("refused store Err = %v, want ErrNotRegistered", err)
	}
	if Refused() != before+1 {
		t.Fatalf("Refused = %d, want %d after one Err", Refused(), before+1)
	}
	for name, s := range map[string]Store{"monitored": NewMonitored(&fakeDB{}),
		"zero": {}, "meta": mustMeta(t, &fakeDB{}, 4)} {
		if err := s.Err(); err != nil {
			t.Fatalf("%s store Err = %v, want nil", name, err)
		}
	}
	if Refused() != before+1 {
		t.Fatalf("Refused = %d: a usable store's Err counted a refusal", Refused())
	}
}
