package histstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeDB records the statements it is sent. It is a DB for the binding
// and registry tests, which never need a server.
type fakeDB struct {
	mu   sync.Mutex
	sql  []string
	args [][]any
}

func (f *fakeDB) record(sql string, args []any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sql = append(f.sql, sql)
	f.args = append(f.args, args)
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.record(sql, args)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.record(sql, args)
	return nil, errors.New("fake: no rows")
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.record(sql, args)
	return errRow{errors.New("fake: no row")}
}

func (f *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("fake: no transactions")
}

// execOnlyDB has Exec only, like the tests' counting wrappers.
type execOnlyDB struct{ f *fakeDB }

func (e execOnlyDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag,
	error) {
	return e.f.Exec(ctx, sql, args...)
}

func mustMeta(t *testing.T, db DB, id int) Store {
	t.Helper()
	s, err := NewMeta(db, id)
	if err != nil {
		t.Fatalf("NewMeta(%d): %v", id, err)
	}
	return s
}

func TestBindMonitoredRemovesEveryMarker(t *testing.T) {
	s := NewMonitored(&fakeDB{})
	sql, args := s.Bind(`SELECT 1 FROM sage.snapshots s WHERE s.category = $1 AND {db:s}`,
		"system")
	want := `SELECT 1 FROM sage.snapshots s WHERE s.category = $1 AND true`
	if sql != want {
		t.Fatalf("monitored bind:\n got %q\nwant %q", sql, want)
	}
	if len(args) != 1 || args[0] != "system" {
		t.Fatalf("monitored bind must not add an argument, got %v", args)
	}
	ins, insArgs := s.Bind(`INSERT INTO sage.query_store (queryid{dbcol}) VALUES ($1{dbval})`,
		int64(42))
	if ins != `INSERT INTO sage.query_store (queryid) VALUES ($1)` {
		t.Fatalf("monitored insert bind: %q", ins)
	}
	if len(insArgs) != 1 {
		t.Fatalf("monitored insert args: %v", insArgs)
	}
}

func TestBindMetaScopesEveryMarkerWithOneArgument(t *testing.T) {
	s := mustMeta(t, &fakeDB{}, 7)
	sql, args := s.Bind(`SELECT 1 FROM sage.snapshots s JOIN sage.snapshots d ON d.base_id = s.id
		WHERE s.category = $1 AND s.collected_at > $2 AND {db:s} AND {db:d}`, "system", 5)
	if !strings.Contains(sql, "s.database_id = $3") ||
		!strings.Contains(sql, "d.database_id = $3") {
		t.Fatalf("meta bind must scope both aliases with $3:\n%s", sql)
	}
	if strings.Contains(sql, "{db:") {
		t.Fatalf("a marker survived binding:\n%s", sql)
	}
	if len(args) != 3 || args[2] != 7 {
		t.Fatalf("meta bind must append the database id once, got %v", args)
	}
	ins, insArgs := s.Bind(`INSERT INTO sage.query_store (queryid{dbcol}) VALUES ($1{dbval})`,
		int64(42))
	if ins != `INSERT INTO sage.query_store (queryid, database_id) VALUES ($1, $2)` {
		t.Fatalf("meta insert bind: %q", ins)
	}
	if len(insArgs) != 2 || insArgs[1] != 7 {
		t.Fatalf("meta insert args: %v", insArgs)
	}
}

func TestBindUnqualifiedMarker(t *testing.T) {
	s := mustMeta(t, &fakeDB{}, 3)
	sql, _ := s.Bind(`SELECT count(*) FROM sage.snapshots WHERE {db:}`)
	if sql != `SELECT count(*) FROM sage.snapshots WHERE database_id = $1` {
		t.Fatalf("unqualified marker: %q", sql)
	}
	legacy := Store{db: &fakeDB{}, legacyNull: true}
	sql, args := legacy.Bind(`SELECT 1 FROM sage.snapshots s WHERE {db:s} AND {db:}`)
	if sql != `SELECT 1 FROM sage.snapshots s WHERE s.database_id IS NULL AND `+
		`database_id IS NULL` {
		t.Fatalf("monitored store on a table with database_id: %q", sql)
	}
	if len(args) != 0 {
		t.Fatalf("IS NULL scoping takes no argument: %v", args)
	}
}

func TestBindWithoutMarkersLeavesStatementAlone(t *testing.T) {
	s := mustMeta(t, &fakeDB{}, 7)
	in := `SELECT '{}'::jsonb, '{db}'::text, $1`
	sql, args := s.Bind(in, 1)
	if sql != in || len(args) != 1 {
		t.Fatalf("no marker: got %q %v", sql, args)
	}
}

func TestNewMetaRefusesMissingIdentity(t *testing.T) {
	for _, id := range []int{0, -1} {
		if _, err := NewMeta(&fakeDB{}, id); !errors.Is(err, ErrNoDatabaseID) {
			t.Fatalf("NewMeta(%d): want ErrNoDatabaseID, got %v", id, err)
		}
	}
	if _, err := NewMeta(nil, 7); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("NewMeta(nil): want ErrNoDatabase, got %v", err)
	}
	var typedNil *pgxpool.Pool
	if _, err := NewMeta(typedNil, 7); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("NewMeta(typed nil pool): want ErrNoDatabase, got %v", err)
	}
}

func TestStoreAccessors(t *testing.T) {
	db := &fakeDB{}
	mon := NewMonitored(db)
	if mon.Scoped() || mon.Mode() != ModeMonitored || mon.DatabaseID() != 0 || mon.DB() != db {
		t.Fatalf("monitored accessors: %+v", mon)
	}
	meta := mustMeta(t, db, 9)
	if !meta.Scoped() || meta.Mode() != ModeMeta || meta.DatabaseID() != 9 {
		t.Fatalf("meta accessors: %+v", meta)
	}
	other := &fakeDB{}
	moved := meta.WithDB(other)
	if moved.DB() != other || moved.DatabaseID() != 9 || !moved.Scoped() {
		t.Fatalf("WithDB must keep the scope: %+v", moved)
	}
}

func TestStoreSendsBoundStatements(t *testing.T) {
	db := &fakeDB{}
	s := mustMeta(t, db, 4)
	ctx := context.Background()
	if _, err := s.Exec(ctx, `DELETE FROM sage.query_store q WHERE {db:q}`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	_, _ = s.Query(ctx, `SELECT 1 FROM sage.snapshots s WHERE s.id = $1 AND {db:s}`, 5)
	_ = s.QueryRow(ctx, `SELECT max(id) FROM sage.snapshots s WHERE {db:s}`).Scan(new(int))
	want := []string{
		`DELETE FROM sage.query_store q WHERE q.database_id = $1`,
		`SELECT 1 FROM sage.snapshots s WHERE s.id = $1 AND s.database_id = $2`,
		`SELECT max(id) FROM sage.snapshots s WHERE s.database_id = $1`,
	}
	for i, w := range want {
		if db.sql[i] != w {
			t.Fatalf("statement %d:\n got %q\nwant %q", i, db.sql[i], w)
		}
	}
	if fmt.Sprint(db.args[1]) != "[5 4]" {
		t.Fatalf("query args: %v", db.args[1])
	}
}

func TestStoreWithoutDatabaseFailsLoudly(t *testing.T) {
	ctx := context.Background()
	var typedNil *pgxpool.Pool
	for name, s := range map[string]Store{"zero": {}, "nil": NewMonitored(nil),
		"typed nil": NewMonitored(typedNil)} {
		if _, err := s.Exec(ctx, "SELECT 1"); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("%s: Exec want ErrNoDatabase, got %v", name, err)
		}
		if _, err := s.Query(ctx, "SELECT 1"); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("%s: Query want ErrNoDatabase, got %v", name, err)
		}
		if err := s.QueryRow(ctx, "SELECT 1").Scan(new(int)); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("%s: QueryRow want ErrNoDatabase, got %v", name, err)
		}
		if _, err := s.Begin(ctx); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("%s: Begin want ErrNoDatabase, got %v", name, err)
		}
	}
}

func TestResolveUnregisteredIsMonitored(t *testing.T) {
	db := &fakeDB{}
	s := Resolve(db)
	if s.Scoped() || s.DB() != db {
		t.Fatalf("unregistered pool must resolve to its own database: %+v", s)
	}
	if got := Resolve(nil); got.Scoped() || got.DB() != nil {
		t.Fatalf("Resolve(nil): %+v", got)
	}
	// Anything else resolves to itself, monitored; a statement it cannot run
	// fails loudly instead of reading nothing.
	got := Resolve(struct{}{})
	if got.Scoped() {
		t.Fatalf("Resolve(non-DB) must be monitored: %+v", got)
	}
	if _, err := got.Exec(context.Background(), "SELECT 1"); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("a handle that cannot run statements: want ErrNoDatabase, got %v", err)
	}
	execOnly := execOnlyDB{&fakeDB{}}
	if _, err := Resolve(execOnly).Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("an Exec-only handle (a counting wrapper) must still run: %v", err)
	}
}

func TestResolveRegisteredAndUnregister(t *testing.T) {
	monitored, meta := &fakeDB{}, &fakeDB{}
	unreg := Register(monitored, "app", mustMeta(t, meta, 11))
	got := Resolve(monitored)
	if !got.Scoped() || got.DatabaseID() != 11 || got.DB() != meta {
		t.Fatalf("registered pool must resolve to its meta store: %+v", got)
	}
	found := false
	for _, r := range Registrations() {
		if r.Monitored == monitored {
			found = r.Name == "app" && r.Store.DatabaseID() == 11
		}
	}
	if !found {
		t.Fatalf("Registrations must list the registration with its name: %+v",
			Registrations())
	}
	unreg()
	unreg() // idempotent
	if Resolve(monitored).Scoped() {
		t.Fatal("after unregister the pool must resolve to its own database")
	}
}

func TestStaleUnregisterKeepsNewerRegistration(t *testing.T) {
	monitored := &fakeDB{}
	first := Register(monitored, "app", mustMeta(t, &fakeDB{}, 1))
	second := Register(monitored, "app", mustMeta(t, &fakeDB{}, 2))
	defer second()
	first() // the runtime that registered first stops after its replacement started
	if got := Resolve(monitored); got.DatabaseID() != 2 {
		t.Fatalf("a stale unregister removed the newer registration: %+v", got)
	}
}

func TestResolvePassesStoresThrough(t *testing.T) {
	s := mustMeta(t, &fakeDB{}, 5)
	if got := Resolve(s); got.DatabaseID() != 5 || !got.Scoped() {
		t.Fatalf("Resolve(Store) must return the store: %+v", got)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	for i := 1; i <= 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			monitored := &fakeDB{}
			unreg := Register(monitored, fmt.Sprint(id), mustMeta(t, &fakeDB{}, id))
			if got := Resolve(monitored).DatabaseID(); got != id {
				t.Errorf("goroutine %d resolved database %d", id, got)
			}
			_ = Registrations()
			unreg()
		}(i)
	}
	wg.Wait()
}

func TestModeValidation(t *testing.T) {
	for _, m := range []string{"monitored", "meta"} {
		if _, err := ParseMode(m); err != nil {
			t.Fatalf("ParseMode(%q): %v", m, err)
		}
	}
	if m, err := ParseMode(""); err != nil || m != ModeMonitored {
		t.Fatalf("ParseMode(\"\") must default to monitored: %v %v", m, err)
	}
	for _, bad := range []string{"Meta", "remote", " meta"} {
		if _, err := ParseMode(bad); err == nil {
			t.Fatalf("ParseMode(%q) must fail", bad)
		}
	}
}
