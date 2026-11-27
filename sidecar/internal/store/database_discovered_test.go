//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func requireDiscoveryDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := requireDBWithClean(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.discovered_databases;
		DELETE FROM sage.discovery_sources`); err != nil {
		t.Fatalf("clean discovery tables: %v", err)
	}
	return pool, ctx
}

// discoveredMember creates a cluster member, then a discovered member
// reusing its credentials, and returns both ids.
func discoveredMember(t *testing.T, ctx context.Context, ds *DatabaseStore,
	name string, expires time.Time) (int, int) {
	t.Helper()
	in := validInput()
	in.Name = "main-" + name
	memberID, err := ds.Create(ctx, in, 1)
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	id, err := ds.CreateDiscovered(ctx, memberID, DiscoveredMember{Name: name,
		DatabaseName: "app_" + name, Source: "cluster-a", Owner: "team-db",
		ExpiresAt: expires})
	if err != nil {
		t.Fatalf("CreateDiscovered: %v", err)
	}
	return memberID, id
}

func TestCreateDiscovered_ReusesCredentialsAtObservation(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	ds := NewDatabaseStore(pool, testKey)
	exp := time.Now().Add(72 * time.Hour).Truncate(time.Microsecond)
	memberID, id := discoveredMember(t, ctx, ds, "orders", exp)
	rec, err := ds.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.TrustLevel != "observation" || rec.ExecutionMode != "approval" {
		t.Fatalf("trust/mode = %s/%s", rec.TrustLevel, rec.ExecutionMode)
	}
	if rec.DiscoveredSource != "cluster-a" || rec.Owner != "team-db" ||
		rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(exp) || rec.AdoptedAt != nil {
		t.Fatalf("discovery columns = %+v", rec)
	}
	if rec.DatabaseName != "app_orders" || rec.Host != "db.example.com" ||
		rec.Username != "admin" || rec.MaxConnections != 1 {
		t.Fatalf("copied fields = %+v", rec)
	}
	conn, err := ds.GetConnectionString(ctx, id)
	if err != nil || !strings.Contains(conn, "admin:s3cret@") ||
		!strings.Contains(conn, "/app_orders") {
		t.Fatalf("connection string reuses the member password: %q %v", conn, err)
	}
	member, _ := ds.Get(ctx, memberID)
	if member.DiscoveredSource != "" || member.ExpiresAt != nil {
		t.Fatalf("manual member got discovery columns: %+v", member)
	}
}

func TestCreateDiscovered_InvalidInput(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	ds := NewDatabaseStore(pool, testKey)
	exp := time.Now().Add(time.Hour)
	if _, err := ds.CreateDiscovered(ctx, 999999, DiscoveredMember{Name: "x",
		DatabaseName: "x", Source: "s", Owner: "o", ExpiresAt: exp}); err == nil {
		t.Fatal("a missing credential member was accepted")
	}
	in := validInput()
	memberID, err := ds.Create(ctx, in, 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, m := range []DiscoveredMember{
		{Name: "x", DatabaseName: "x", Source: "s", ExpiresAt: exp},            // no owner
		{Name: "x", DatabaseName: "x", Owner: "o", ExpiresAt: exp},             // no source
		{Name: "x", DatabaseName: "x", Source: "s", Owner: "o"},                // no expiry
		{Name: "", DatabaseName: "x", Source: "s", Owner: "o", ExpiresAt: exp}, // no name
	} {
		if _, err := ds.CreateDiscovered(ctx, memberID, m); !errors.Is(err, ErrValidation) {
			t.Errorf("%+v: err = %v, want ErrValidation", m, err)
		}
	}
}

// CHECK-02: a trust_level change on an unadopted discovered row returns
// ErrDiscoveredTrust on every store path; after adoption it is allowed.
func TestDiscoveredTrustGuard(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	ds := NewDatabaseStore(pool, testKey)
	cs := NewConfigStore(pool)
	memberID, id := discoveredMember(t, ctx, ds, "guard", time.Now().Add(time.Hour))

	if _, err := cs.SetDatabaseTrustPolicy(ctx, id, "advisory"); !errors.Is(err,
		ErrDiscoveredTrust) {
		t.Fatalf("SetDatabaseTrustPolicy err = %v, want ErrDiscoveredTrust", err)
	}
	_, err := cs.SetDatabaseOverridesCAS(ctx,
		[]ConfigOverrideWrite{{Key: "trust.level", Value: "autonomous"}}, id, 0, 1, nil)
	if !errors.Is(err, ErrDiscoveredTrust) {
		t.Fatalf("SetDatabaseOverridesCAS err = %v, want ErrDiscoveredTrust", err)
	}
	auto := "auto"
	_, err = cs.SetDatabaseOverridesCAS(ctx, nil, id, 0, 1, &auto)
	if !errors.Is(err, ErrDiscoveredTrust) {
		t.Fatalf("execution_mode auto err = %v, want ErrDiscoveredTrust", err)
	}
	_, err = pool.Exec(ctx, `UPDATE sage.databases SET discovered_source = NULL,
		trust_level = 'autonomous' WHERE id = $1`, id)
	if !errors.Is(DiscoveredTrustError(err), ErrDiscoveredTrust) {
		t.Fatalf("clearing discovered_source to escape the guard: %v", err)
	}
	rec, _ := ds.Get(ctx, id)
	if rec.TrustLevel != "observation" || rec.ExecutionMode != "approval" {
		t.Fatalf("guarded row changed: %s/%s", rec.TrustLevel, rec.ExecutionMode)
	}
	// A manual member is untouched by the guard.
	if _, err := cs.SetDatabaseTrustPolicy(ctx, memberID, "advisory"); err != nil {
		t.Fatalf("manual member trust change: %v", err)
	}
	// Adoption hands trust to the owner's normal paths.
	ds2 := NewDiscoveryStore(pool)
	rowID := enrolledRow(t, ctx, ds2, id)
	if err := ds2.Adopt(ctx, rowID, 1); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if _, err := cs.SetDatabaseTrustPolicy(ctx, id, "advisory"); err != nil {
		t.Fatalf("trust change after adoption: %v", err)
	}
}

// enrolledRow records a discovered row and marks it enrolled as id.
func enrolledRow(t *testing.T, ctx context.Context, d *DiscoveryStore, id int) int64 {
	t.Helper()
	ext := fmt.Sprintf("h:5432/db%d", id)
	if _, err := d.Upsert(ctx, "cluster-a", "team-db", []DiscoveredCandidate{
		{ExternalID: ext, Host: "h", Port: 5432, DBName: "db"}}, time.Now()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	rows, err := d.Claim(ctx, "cluster-a", 50, 50, time.Now())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, r := range rows {
		if r.ExternalID == ext {
			if err := d.MarkEnrolled(ctx, r.ID, id); err != nil {
				t.Fatalf("MarkEnrolled: %v", err)
			}
			return r.ID
		}
	}
	t.Fatalf("claim did not return %s: %+v", ext, rows)
	return 0
}

func TestDiscoveryStore_UpsertIsIdempotentAndCountsNew(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	d := NewDiscoveryStore(pool)
	now := time.Now()
	c := []DiscoveredCandidate{{ExternalID: "a", Host: "h", Port: 1, DBName: "a"},
		{ExternalID: "b", Host: "h", Port: 1, DBName: "b"}}
	n, err := d.Upsert(ctx, "s", "o", c, now)
	if err != nil || n != 2 {
		t.Fatalf("first upsert: n=%d err=%v", n, err)
	}
	n, err = d.Upsert(ctx, "s", "o2", c, now.Add(time.Minute))
	if err != nil || n != 0 {
		t.Fatalf("second upsert: n=%d err=%v", n, err)
	}
	rows, err := d.List(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("List: %d %v", len(rows), err)
	}
	if rows[0].Owner != "o2" || !rows[0].LastSeen.After(rows[0].FirstSeen) ||
		rows[0].Status != DiscoveryCandidate {
		t.Fatalf("row after re-upsert = %+v", rows[0])
	}
	if n, err := d.Upsert(ctx, "s", "o", nil, now); err != nil || n != 0 {
		t.Fatalf("empty upsert: %d %v", n, err)
	}
}

// Concurrency: two sidecars recording and claiming the same listing make
// one row per candidate and never exceed max_enrolled between them.
func TestDiscoveryStore_ConcurrentSidecarsNoDuplicates(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	a, b := NewDiscoveryStore(pool), NewDiscoveryStore(pool)
	var c []DiscoveredCandidate
	for i := range 6 {
		c = append(c, DiscoveredCandidate{ExternalID: fmt.Sprintf("x%d", i), Host: "h",
			Port: 1, DBName: fmt.Sprintf("x%d", i)})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[int64]int{}
	errs := make(chan error, 8)
	for _, st := range []*DiscoveryStore{a, b, a, b} {
		wg.Add(1)
		go func(st *DiscoveryStore) {
			defer wg.Done()
			if _, err := st.Upsert(ctx, "s", "o", c, time.Now()); err != nil {
				errs <- err
				return
			}
			rows, err := st.Claim(ctx, "s", 5, 3, time.Now())
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			for _, r := range rows {
				claimed[r.ID]++
			}
			mu.Unlock()
		}(st)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent store call: %v", err)
	}
	assertClaimedOnce(t, ctx, pool, claimed)
}

func assertClaimedOnce(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	claimed map[int64]int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.discovered_databases
		WHERE source = 's'`).Scan(&n); err != nil || n != 6 {
		t.Fatalf("rows = %d (%v), want 6", n, err)
	}
	if len(claimed) != 3 {
		t.Fatalf("claimed %d rows across sidecars, want max_enrolled 3", len(claimed))
	}
	for id, times := range claimed {
		if times != 1 {
			t.Fatalf("row %d claimed %d times", id, times)
		}
	}
}

func TestDiscoveryStore_ClaimSkipsAlreadyManaged(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	ds := NewDatabaseStore(pool, testKey)
	in := validInput() // db.example.com:5432/myapp
	if _, err := ds.Create(ctx, in, 1); err != nil {
		t.Fatalf("create: %v", err)
	}
	d := NewDiscoveryStore(pool)
	_, _ = d.Upsert(ctx, "s", "o", []DiscoveredCandidate{
		{ExternalID: "m", Host: "db.example.com", Port: 5432, DBName: "myapp"},
		{ExternalID: "n", Host: "db.example.com", Port: 5432, DBName: "other"}}, time.Now())
	rows, err := d.Claim(ctx, "s", 5, 5, time.Now())
	if err != nil || len(rows) != 1 || rows[0].ExternalID != "n" {
		t.Fatalf("claim = %+v %v, want only n", rows, err)
	}
	list, _ := d.List(ctx)
	for _, r := range list {
		if r.ExternalID == "m" && !r.AlreadyManaged {
			t.Fatalf("m not flagged already managed: %+v", r)
		}
	}
}

func TestDiscoveryStore_GoneAndSourceErrors(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	d := NewDiscoveryStore(pool)
	t0 := time.Now().Add(-time.Hour)
	_, _ = d.Upsert(ctx, "s", "o", []DiscoveredCandidate{{ExternalID: "a"},
		{ExternalID: "b"}}, t0)
	_, _ = d.Upsert(ctx, "s", "o", []DiscoveredCandidate{{ExternalID: "a"}}, t0.Add(time.Minute))
	gone, err := d.MarkGone(ctx, "s", t0.Add(time.Minute))
	if err != nil || len(gone) != 1 || gone[0].ExternalID != "b" ||
		gone[0].Status != DiscoveryGone {
		t.Fatalf("MarkGone = %+v %v", gone, err)
	}
	if again, _ := d.MarkGone(ctx, "s", t0.Add(time.Minute)); len(again) != 0 {
		t.Fatalf("gone marked twice: %+v", again)
	}
	denied := errors.New("AccessDenied: 403")
	if err := d.RecordSourceRun(ctx, "s", 0, denied, time.Now()); err != nil {
		t.Fatalf("RecordSourceRun: %v", err)
	}
	srcs, err := d.Sources(ctx)
	if err != nil || len(srcs) != 1 || !strings.Contains(srcs[0].LastError, "403") ||
		srcs[0].LastSuccessAt != nil {
		t.Fatalf("sources after 403 = %+v %v", srcs, err)
	}
	if err := d.RecordSourceRun(ctx, "s", 1, nil, time.Now()); err != nil {
		t.Fatalf("RecordSourceRun ok: %v", err)
	}
	srcs, _ = d.Sources(ctx)
	if srcs[0].LastError != "" || srcs[0].LastSuccessAt == nil || srcs[0].LastCount != 1 {
		t.Fatalf("sources after success = %+v", srcs[0])
	}
}

// Boundary and state transitions: enrolled, then expired at expires_at
// exactly; adopted rows never expire; ignore and extend check state.
func TestDiscoveryStore_ExpiryAdoptIgnoreExtend(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	ds, d := NewDatabaseStore(pool, testKey), NewDiscoveryStore(pool)
	exp := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	_, idA := discoveredMember(t, ctx, ds, "a", exp)
	_, idB := discoveredMember(t, ctx, ds, "b", exp)
	rowA, rowB := enrolledRow(t, ctx, d, idA), enrolledRow(t, ctx, d, idB)

	if due, _ := d.DueForExpiry(ctx, exp.Add(-time.Microsecond)); len(due) != 0 {
		t.Fatalf("due before expires_at: %+v", due)
	}
	if err := d.Adopt(ctx, rowB, 1); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	due, err := d.DueForExpiry(ctx, exp)
	if err != nil || len(due) != 1 || due[0].ID != rowA || *due[0].DatabaseID != idA {
		t.Fatalf("due at expires_at = %+v %v", due, err)
	}
	if err := ds.DisableDiscovered(ctx, idA); err != nil {
		t.Fatalf("DisableDiscovered: %v", err)
	}
	if err := d.SetStatus(ctx, rowA, DiscoveryExpired, "ttl"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	recA, _ := ds.Get(ctx, idA)
	recB, _ := ds.Get(ctx, idB)
	if recA.Enabled || !recB.Enabled || recB.AdoptedAt == nil || recB.ExpiresAt != nil {
		t.Fatalf("after expiry/adopt: a=%+v b=%+v", recA, recB)
	}
	if err := d.Adopt(ctx, rowA, 1); !errors.Is(err, ErrValidation) {
		t.Fatalf("adopting an expired row: %v", err)
	}
	if _, err := d.ExtendTTL(ctx, rowB, 24, 1); !errors.Is(err, ErrValidation) {
		t.Fatalf("extending an adopted row: %v", err)
	}
	if err := d.Ignore(ctx, rowB, 1); !errors.Is(err, ErrValidation) {
		t.Fatalf("ignoring an adopted row: %v", err)
	}
	if err := d.Adopt(ctx, 987654, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("adopting a missing row: %v", err)
	}
	var audits int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE key LIKE 'fleet.discovery.%' AND database_id IN ($1, $2)`,
		idA, idB).Scan(&audits)
	if audits < 4 { // enrol a, enrol b, adopt b, expired a
		t.Fatalf("discovery audit rows = %d, want >= 4", audits)
	}
}

func TestDiscoveryStore_ExtendTTLBoundaries(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	ds, d := NewDatabaseStore(pool, testKey), NewDiscoveryStore(pool)
	exp := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	_, id := discoveredMember(t, ctx, ds, "ext", exp)
	row := enrolledRow(t, ctx, d, id)
	for _, h := range []int{0, -1, 721} {
		if _, err := d.ExtendTTL(ctx, row, h, 1); !errors.Is(err, ErrValidation) {
			t.Errorf("ExtendTTL(%d) err = %v", h, err)
		}
	}
	got, err := d.ExtendTTL(ctx, row, 24, 1)
	if err != nil || !got.Equal(exp.Add(24*time.Hour)) {
		t.Fatalf("ExtendTTL = %v %v, want %v", got, err, exp.Add(24*time.Hour))
	}
	rec, _ := ds.Get(ctx, id)
	if rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(got) {
		t.Fatalf("stored expires_at = %v", rec.ExpiresAt)
	}
}

func TestDiscoveryStore_IgnoreCandidate(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	d := NewDiscoveryStore(pool)
	_, _ = d.Upsert(ctx, "s", "o", []DiscoveredCandidate{{ExternalID: "a"}}, time.Now())
	rows, _ := d.List(ctx)
	if err := d.Ignore(ctx, rows[0].ID, 1); err != nil {
		t.Fatalf("Ignore: %v", err)
	}
	if claimed, _ := d.Claim(ctx, "s", 5, 5, time.Now()); len(claimed) != 0 {
		t.Fatalf("ignored row claimed: %+v", claimed)
	}
	if gone, _ := d.MarkGone(ctx, "s", time.Now().Add(time.Hour)); len(gone) != 0 {
		t.Fatalf("ignored row marked gone: %+v", gone)
	}
}

// Boundary: five candidates whose enrolment keeps failing do not starve a
// sixth that was never tried; failed rows retry longest-waiting first.
func TestDiscoveryStore_ClaimPrefersUntriedOverFailed(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	d := NewDiscoveryStore(pool)
	now := time.Now()
	for i := range 6 {
		c := []DiscoveredCandidate{{ExternalID: fmt.Sprintf("f%d", i), Host: "h",
			Port: 1, DBName: fmt.Sprintf("f%d", i)}}
		seen := now.Add(time.Duration(i) * time.Second)
		if _, err := d.Upsert(ctx, "s", "o", c, seen); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	first, err := d.Claim(ctx, "s", 5, 10, now)
	if err != nil || len(first) != 5 || first[0].ExternalID != "f0" {
		t.Fatalf("first claim = %+v %v, want f0-f4", first, err)
	}
	for _, r := range first {
		if err := d.Release(ctx, r.ID, "permission denied for database"); err != nil {
			t.Fatalf("release %s: %v", r.ExternalID, err)
		}
	}
	second, err := d.Claim(ctx, "s", 2, 10, now)
	if err != nil || len(second) != 2 {
		t.Fatalf("second claim = %+v %v", second, err)
	}
	if second[0].ExternalID != "f0" && second[1].ExternalID != "f0" ||
		second[0].ExternalID != "f5" && second[1].ExternalID != "f5" {
		t.Fatalf("second claim = %s, %s; want the untried f5 and the oldest failure f0",
			second[0].ExternalID, second[1].ExternalID)
	}
}

// Concurrency: sidecars listing the same rows in different orders while
// others claim and mark gone never deadlock (40P01); discoveryLock orders
// every multi-row write.
func TestDiscoveryStore_ConcurrentWritesNeverDeadlock(t *testing.T) {
	pool, ctx := requireDiscoveryDB(t)
	var asc, desc []DiscoveredCandidate
	for i := range 30 {
		c := DiscoveredCandidate{ExternalID: fmt.Sprintf("r%02d", i), Host: "h", Port: 1,
			DBName: fmt.Sprintf("r%02d", i)}
		asc = append(asc, c)
		desc = append([]DiscoveredCandidate{c}, desc...)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	work := func(i int) error {
		d := NewDiscoveryStore(pool)
		switch i % 4 {
		case 0:
			_, err := d.Upsert(ctx, "s", "o", asc, time.Now())
			return err
		case 1:
			_, err := d.Upsert(ctx, "s", "o", desc, time.Now())
			return err
		case 2:
			_, err := d.Claim(ctx, "s", 5, 50, time.Now())
			return err
		default:
			_, err := d.MarkGone(ctx, "s", time.Now().Add(-time.Hour))
			return err
		}
	}
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for round := range 15 {
				if err := work(g + round); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent discovery write: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.discovered_databases
		WHERE source = 's'`).Scan(&n); err != nil || n != 30 {
		t.Fatalf("rows = %d (%v), want 30", n, err)
	}
}
