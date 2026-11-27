package collector

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// vacuum-01: a partitioned parent has relfrozenxid 0 and relminmxid 0, and
// age() of a non-normal XID is 2147483647. The tables page reads such an
// age as unknown (-1) and reports each row's relkind; leaves, plain tables
// and materialized views keep their own (small, fresh-fixture) ages.
func TestCollectTables_PartitionedParentAgesUnknown(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	createGoldenCatalog(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	got, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables: %v", err)
	}
	gold := goldenTables(got)
	wantKind := map[string]string{
		"golden_cat.hot": "r", "golden_cat.noidx": "r", "golden_cat.unlogged": "r",
		"golden_cat.parted": "p", "golden_cat.parted_p1": "r",
		"golden_cat.parted_p2": "r", "golden_cat.mv": "m",
	}
	for key, kind := range wantKind {
		if ts, ok := gold[key]; !ok || ts.Relkind != kind {
			t.Errorf("%s relkind %q (collected %v), want %q", key, ts.Relkind, ok, kind)
		}
	}
	parent := gold["golden_cat.parted"]
	if parent.XIDAge != -1 || parent.MXIDAge != -1 {
		t.Fatalf("partitioned parent xid_age %d mxid_age %d, want -1/-1 (unknown), "+
			"not 2147483647", parent.XIDAge, parent.MXIDAge)
	}
	for key, kind := range wantKind {
		ts := gold[key]
		if kind == "p" {
			continue
		}
		if ts.XIDAge < 0 || ts.XIDAge >= 1_000_000 || ts.MXIDAge < 0 ||
			ts.MXIDAge >= 1_000_000 {
			t.Errorf("%s xid_age %d mxid_age %d, want a fresh table's small ages",
				key, ts.XIDAge, ts.MXIDAge)
		}
	}
	// CHECK-02: no collected row carries the non-normal-XID sentinel.
	raw := asJSON(t, got)
	if strings.Contains(raw, `"xid_age":2147483647`) ||
		strings.Contains(raw, `"mxid_age":2147483647`) {
		t.Fatalf("collected tables still carry age 2147483647: %s", raw)
	}
}

// vacuum-01: the row's XID age is the older of the heap's and its TOAST
// table's. VACUUM (FREEZE, PROCESS_TOAST false) advances only the heap
// (PG14+), leaving the TOAST relation older; the collected age must be the
// TOAST age, which is what autovacuum's anti-wraparound pass acts on. It
// runs on the exclusive server: on the shared one other packages' XID burns
// outrun the 5000 slack between the two age reads, and on PostgreSQL 14 a
// backend still logging in holds back the horizon the heap-only freeze needs.
func TestCollectTables_ToastOlderThanHeapReadsToastAge(t *testing.T) {
	pool := exclusivePool(t, "the TOAST age a heap-only freeze leaves behind")
	ctx := context.Background()
	heapAge, toastAge := createToastOlderThanHeap(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	got, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables: %v", err)
	}
	var doc *TableStats
	for i := range got {
		if got[i].SchemaName == "golden_toast" && got[i].RelName == "doc" {
			doc = &got[i]
		}
	}
	if doc == nil {
		t.Fatal("golden_toast.doc not collected")
	}
	if doc.XIDAge < toastAge || doc.XIDAge-toastAge > 5000 {
		t.Fatalf("xid_age %d, want the TOAST age %d (heap %d)", doc.XIDAge, toastAge,
			heapAge)
	}
}

// createToastOlderThanHeap builds golden_toast.doc whose TOAST relation is
// at least 250 transactions older than its heap, and returns both ages.
func createToastOlderThanHeap(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) (heapAge, toastAge int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS golden_toast CASCADE")
	})
	for _, sql := range []string{
		"DROP SCHEMA IF EXISTS golden_toast CASCADE",
		"CREATE SCHEMA golden_toast",
		`CREATE TABLE golden_toast.doc (id int, body text)
		   WITH (autovacuum_enabled = off, toast.autovacuum_enabled = off)`,
		`INSERT INTO golden_toast.doc SELECT g, (SELECT string_agg(md5(g::text || r::text),
		   '') FROM generate_series(1, 300) r) FROM generate_series(1, 20) g`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for i := 0; i < 300; i++ { // each statement is its own transaction: one XID
		if _, err := pool.Exec(ctx, "SELECT txid_current()"); err != nil {
			t.Fatalf("burn xid: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		"VACUUM (FREEZE, PROCESS_TOAST false) golden_toast.doc"); err != nil {
		t.Fatalf("freeze heap only: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT age(c.relfrozenxid), age(t.relfrozenxid)
		FROM pg_class c JOIN pg_class t ON t.oid = c.reltoastrelid
		WHERE c.oid = 'golden_toast.doc'::regclass`).Scan(&heapAge, &toastAge); err != nil {
		t.Fatalf("read ages: %v", err)
	}
	if toastAge < heapAge+250 {
		t.Fatalf("fixture: toast age %d not older than heap age %d", toastAge, heapAge)
	}
	return heapAge, toastAge
}
