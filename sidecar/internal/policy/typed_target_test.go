package policy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Typed-target leases (debt item 2): a lease names the exact object it
// changes (OID, name and kind), so a rename, a quoting difference or an
// index of the leased table cannot slip past another writer's lease.

type typedFixture struct {
	pool     *pgxpool.Pool
	table    string // unqualified, lower case
	index    string
	tableOID uint32
	indexOID uint32
}

func newTypedFixture(t *testing.T, store *Store) typedFixture {
	t.Helper()
	ctx := context.Background()
	f := typedFixture{pool: store.pool,
		table: fmt.Sprintf("typed_lease_%d", time.Now().UnixNano())}
	f.index = f.table + "_a_idx"
	for _, statement := range []string{
		"CREATE TABLE public." + f.table + " (id int, a int)",
		"CREATE INDEX " + f.index + " ON public." + f.table + " (a)",
	} {
		if _, err := f.pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+f.table)
		_, _ = f.pool.Exec(context.Background(),
			"DROP TABLE IF EXISTS public."+f.table+"_renamed")
	})
	f.tableOID = relationOID(t, f.pool, "public."+f.table)
	f.indexOID = relationOID(t, f.pool, "public."+f.index)
	return f
}

func relationOID(t *testing.T, pool *pgxpool.Pool, name string) uint32 {
	t.Helper()
	var oid uint32
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass($1)::oid",
		name).Scan(&oid); err != nil || oid == 0 {
		t.Fatalf("oid of %s = %d, %v", name, oid, err)
	}
	return oid
}

func oidKey(oid uint32) string { return "oid:" + strconv.FormatUint(uint64(oid), 10) }

func typedDecision(t *testing.T, store *Store) int64 {
	t.Helper()
	policyRow := bootstrapPolicy(t, store, Scope{})
	var id int64
	err := store.pool.QueryRow(context.Background(), `INSERT INTO sage.decision
		(feature,intent,policy_version,verdict,risk_tier,reason,evidence_id)
		VALUES ('schema_change','typed lease test',$1,'execute','moderate','test',$2)
		RETURNING id`, policyRow.Version,
		fmt.Sprintf("typed-lease-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return id
}

func mustResolve(t *testing.T, pool *pgxpool.Pool, names ...string) []TypedTarget {
	t.Helper()
	targets, err := ResolveTypedTargets(context.Background(), pool, names)
	if err != nil {
		t.Fatalf("resolve %v: %v", names, err)
	}
	return targets
}

func hasKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

func TestResolveTypedTargetsTable(t *testing.T) {
	f := newTypedFixture(t, newTestStore(t))

	targets := mustResolve(t, f.pool, "public."+f.table)

	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want exactly the table", targets)
	}
	got := targets[0]
	if got.Kind != TargetTable || got.Schema != "public" || got.Name != f.table ||
		got.OID != f.tableOID {
		t.Fatalf("target = %+v, want table public.%s oid %d", got, f.table, f.tableOID)
	}
	keys := TypedLeaseKeys(targets)
	if len(keys) != 2 || !hasKey(keys, "public."+f.table) || !hasKey(keys, oidKey(f.tableOID)) {
		t.Fatalf("lease keys = %v, want the name key and %s", keys, oidKey(f.tableOID))
	}
}

func TestResolveTypedTargetsIndexIncludesParentTable(t *testing.T) {
	f := newTypedFixture(t, newTestStore(t))

	targets := mustResolve(t, f.pool, "public."+f.index)

	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want the index and its table", targets)
	}
	if targets[0].Kind != TargetIndex || targets[0].OID != f.indexOID ||
		targets[0].Name != f.index {
		t.Fatalf("first target = %+v, want index %s", targets[0], f.index)
	}
	if targets[1].Kind != TargetTable || targets[1].OID != f.tableOID {
		t.Fatalf("second target = %+v, want parent table oid %d", targets[1], f.tableOID)
	}
	keys := TypedLeaseKeys(targets)
	if !hasKey(keys, oidKey(f.indexOID)) || !hasKey(keys, oidKey(f.tableOID)) {
		t.Fatalf("lease keys = %v, want both the index and table OIDs", keys)
	}
}

func TestResolveTypedTargetsQuotedMixedCase(t *testing.T) {
	store := newTestStore(t)
	name := fmt.Sprintf("Typed_Mixed_%d", time.Now().UnixNano())
	if _, err := store.pool.Exec(context.Background(),
		`CREATE TABLE public."`+name+`" (id int)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DROP TABLE IF EXISTS public."`+name+`"`)
	})

	targets := mustResolve(t, store.pool, `public."`+name+`"`)

	if len(targets) != 1 || targets[0].Name != name || targets[0].Kind != TargetTable {
		t.Fatalf("targets = %+v, want table %q", targets, name)
	}
	want := `public."` + name + `"`
	if targets[0].Canonical() != want {
		t.Fatalf("canonical = %q, want %q (the NormalizeTargetObjects form)",
			targets[0].Canonical(), want)
	}
}

func TestResolveTypedTargetsUnqualifiedUsesSearchPath(t *testing.T) {
	f := newTypedFixture(t, newTestStore(t))

	targets := mustResolve(t, f.pool, f.table)

	if len(targets) != 1 || targets[0].Schema != "public" || targets[0].OID != f.tableOID {
		t.Fatalf("targets = %+v, want public.%s through search_path", targets, f.table)
	}
}

func TestResolveTypedTargetsMissingObjects(t *testing.T) {
	store := newTestStore(t)
	missing := fmt.Sprintf("no_such_rel_%d", time.Now().UnixNano())

	targets := mustResolve(t, store.pool, "public."+missing)

	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want the qualified missing name", targets)
	}
	got := targets[0]
	if got.Kind != TargetUnresolved || got.OID != 0 || got.Canonical() != "public."+missing {
		t.Fatalf("missing target = %+v, want unresolved public.%s", got, missing)
	}
	if keys := got.LeaseKeys(); len(keys) != 1 || keys[0] != "public."+missing {
		t.Fatalf("missing target keys = %v, want only its name key", keys)
	}
	// An unqualified missing name has no name key: leasing nothing would
	// let the change run unleased, so it is an error.
	if _, err := ResolveTypedTargets(context.Background(), store.pool,
		[]string{missing}); err == nil {
		t.Fatalf("unqualified missing %q resolved, want an error", missing)
	}
}

func TestResolveTypedTargetsRejectsInvalidInput(t *testing.T) {
	store := newTestStore(t)
	for _, name := range []string{"a.b.c", `"unterminated`, "public.t;drop", " ", ""} {
		if _, err := ResolveTypedTargets(context.Background(), store.pool,
			[]string{name}); err == nil {
			t.Errorf("ResolveTypedTargets(%q) succeeded, want an invalid-name error", name)
		}
	}
	targets, err := ResolveTypedTargets(context.Background(), store.pool, nil)
	if err != nil || len(targets) != 0 {
		t.Fatalf("empty input = %v, %v; want no targets and no error", targets, err)
	}
	if _, err := ResolveTypedTargets(context.Background(), nil,
		[]string{"public.t"}); err == nil {
		t.Fatal("nil resolver succeeded, want an unavailable-database error")
	}
}

func TestTypedLeaseConflictsAcrossRename(t *testing.T) {
	store := newTestStore(t)
	f := newTypedFixture(t, store)
	decision := typedDecision(t, store)
	first := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)
	held, err := first.AcquireTyped(context.Background(), "custodian",
		mustResolve(t, f.pool, "public."+f.table), "VACUUM (FREEZE) public."+f.table)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = first.ReleaseLease(context.Background(), held) }()
	if _, err := f.pool.Exec(context.Background(), "ALTER TABLE public."+f.table+
		" RENAME TO "+f.table+"_renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	second := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)
	_, err = second.AcquireTyped(context.Background(), "operator:user:7",
		mustResolve(t, f.pool, "public."+f.table+"_renamed"), "ALTER TABLE ...")

	var conflict *LeaseConflictError
	if !errors.Is(err, ErrLeaseConflict) || !errors.As(err, &conflict) {
		t.Fatalf("acquire after rename = %v, want a LeaseConflictError", err)
	}
	if conflict.Holder != "custodian" || conflict.DecisionID != decision {
		t.Fatalf("conflict = %+v, want holder custodian decision %d", conflict, decision)
	}
}

func TestTypedLeaseOnIndexConflictsWithTableLease(t *testing.T) {
	store := newTestStore(t)
	f := newTypedFixture(t, store)
	decision := typedDecision(t, store)
	table := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)
	held, err := table.AcquireTyped(context.Background(), "custodian",
		mustResolve(t, f.pool, "public."+f.table), "ALTER TABLE")
	if err != nil {
		t.Fatalf("table lease: %v", err)
	}

	index := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)
	_, err = index.AcquireTyped(context.Background(), "operator",
		mustResolve(t, f.pool, "public."+f.index), "DROP INDEX")

	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("index lease while its table is leased = %v, want ErrLeaseConflict", err)
	}
	if err := table.ReleaseLease(context.Background(), held); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := index.AcquireTyped(context.Background(), "operator",
		mustResolve(t, f.pool, "public."+f.index), "DROP INDEX")
	if err != nil {
		t.Fatalf("index lease after release = %v, want granted", err)
	}
	_ = index.ReleaseLease(context.Background(), again)
}

func TestTypedLeaseDisjointObjectsDoNotConflict(t *testing.T) {
	store := newTestStore(t)
	one, two := newTypedFixture(t, store), newTypedFixture(t, store)
	decision := typedDecision(t, store)
	manager := NewPostgresLeaseManager(store.pool, nil, decision, time.Minute)

	a, errA := manager.AcquireTyped(context.Background(), "executor",
		mustResolve(t, store.pool, "public."+one.table), "ANALYZE")
	b, errB := manager.AcquireTyped(context.Background(), "executor",
		mustResolve(t, store.pool, "public."+two.table), "ANALYZE")

	if errA != nil || errB != nil {
		t.Fatalf("disjoint leases = %v / %v, want both granted", errA, errB)
	}
	_ = manager.ReleaseLease(context.Background(), a)
	_ = manager.ReleaseLease(context.Background(), b)
}

func TestTypedLeaseRecordsIdentityAndReleases(t *testing.T) {
	store := newTestStore(t)
	f := newTypedFixture(t, store)
	decision := typedDecision(t, store)
	manager := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)

	leaseID, err := manager.AcquireTyped(context.Background(), "operator:user:3",
		mustResolve(t, f.pool, "public."+f.table), "ALTER TABLE x")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var actor, objectType, objectName string
	var objectOID uint32
	err = f.pool.QueryRow(context.Background(), `SELECT actor, object_type,
		object_oid, object_name FROM sage.change_lease
		WHERE decision_id=$1 AND object_key=$2 AND state='active'`,
		decision, oidKey(f.tableOID)).Scan(&actor, &objectType, &objectOID, &objectName)
	if err != nil {
		t.Fatalf("read typed lease row: %v", err)
	}
	if actor != "operator:user:3" || objectType != "table" || objectOID != f.tableOID ||
		objectName != "public."+f.table {
		t.Fatalf("lease row = %s %s %d %s, want the operator and exact table identity",
			actor, objectType, objectOID, objectName)
	}
	if err := manager.ReleaseLease(context.Background(), leaseID); err != nil {
		t.Fatalf("release: %v", err)
	}
	var active int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.change_lease
		WHERE decision_id=$1 AND state='active'`, decision).Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 0 {
		t.Fatalf("active lease rows after release = %d, want 0", active)
	}
}

func TestNameLeaseConflictsWithTypedLease(t *testing.T) {
	store := newTestStore(t)
	f := newTypedFixture(t, store)
	decision := typedDecision(t, store)
	legacy := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)
	objects, err := NormalizeTargetObjects([]string{"public." + f.table})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	held, err := legacy.AcquireLease(context.Background(), "executor", objects, "ALTER")
	if err != nil {
		t.Fatalf("name lease: %v", err)
	}
	defer func() { _ = legacy.ReleaseLease(context.Background(), held) }()

	typed := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)
	_, err = typed.AcquireTyped(context.Background(), "operator",
		mustResolve(t, f.pool, "public."+f.table), "ALTER")

	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("typed lease over a name lease = %v, want ErrLeaseConflict", err)
	}
}

// Many writers race for one object: exactly one holds it.
func TestTypedLeaseConcurrentAcquireIsExclusive(t *testing.T) {
	store := newTestStore(t)
	f := newTypedFixture(t, store)
	decision := typedDecision(t, store)
	targets := mustResolve(t, f.pool, "public."+f.table)
	const writers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted, conflicts := []LeaseID{}, 0
	managers := make([]*PostgresLeaseManager, writers)
	for i := range managers {
		managers[i] = NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)
		wg.Add(1)
		go func(manager *PostgresLeaseManager) {
			defer wg.Done()
			id, err := manager.AcquireTyped(context.Background(), "writer", targets, "x")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				granted = append(granted, id)
			} else if errors.Is(err, ErrLeaseConflict) {
				conflicts++
			} else {
				t.Errorf("unexpected acquire error: %v", err)
			}
		}(managers[i])
	}
	wg.Wait()

	if len(granted) != 1 || conflicts != writers-1 {
		t.Fatalf("granted=%d conflicts=%d, want exactly one holder", len(granted), conflicts)
	}
	for _, manager := range managers {
		_ = manager.ReleaseLease(context.Background(), granted[0])
	}
}

func TestAcquireTypedRejectsEmptyTargetsAndActor(t *testing.T) {
	store := newTestStore(t)
	f := newTypedFixture(t, store)
	decision := typedDecision(t, store)
	manager := NewPostgresLeaseManager(f.pool, nil, decision, time.Minute)

	if _, err := manager.AcquireTyped(context.Background(), "x", nil, "intent"); err == nil {
		t.Fatal("empty targets acquired a lease")
	}
	if _, err := manager.AcquireTyped(context.Background(), " ",
		mustResolve(t, f.pool, "public."+f.table), "intent"); err == nil {
		t.Fatal("blank actor acquired a lease")
	}
}
