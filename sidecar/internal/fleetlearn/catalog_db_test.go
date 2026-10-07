package fleetlearn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReadFingerprint_SameShapeDifferentNamesMatch(t *testing.T) {
	ctx := context.Background()
	a := freshDB(t, "fp_a")
	b := freshDB(t, "fp_b")
	tenantSchema(t, a, "orders", "customers")
	tenantSchema(t, b, "purchases", "clients")

	fa, ta, err := ReadFingerprint(ctx, a, ReadOptions{Database: "a"})
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	fb, _, err := ReadFingerprint(ctx, b, ReadOptions{Database: "b"})
	if err != nil {
		t.Fatalf("read b: %v", err)
	}
	if len(fa.Tables) != 2 || len(fa.Indexes) != 4 {
		t.Fatalf("a: tables=%d indexes=%d, want 2 and 4 (2 pkeys + 2)",
			len(fa.Tables), len(fa.Indexes))
	}
	if strings.Join(fa.Tables, ",") != strings.Join(fb.Tables, ",") ||
		strings.Join(fa.Indexes, ",") != strings.Join(fb.Indexes, ",") {
		t.Fatalf("same shapes under different names differ:\n%+v\n%+v", fa, fb)
	}
	if fa.Database != "a" || fa.ComputedAt.IsZero() {
		t.Fatalf("metadata not set: %+v", fa)
	}
	if ta["public.orders"] == "" || ta["public.customers"] == "" {
		t.Fatalf("table index missing tables: %v", ta)
	}
	for _, sageTable := range []string{"sage.findings", "sage.action_log"} {
		if _, has := ta[sageTable]; has {
			t.Fatalf("pg_sage's own schema leaked into the fingerprint: %s", sageTable)
		}
	}
}

func TestReadFingerprint_IsPrivateByDefault(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "fp_private")
	tenantSchema(t, pool, "secret_orders", "secret_customers")
	exec(t, pool, "INSERT INTO secret_customers VALUES (1,'alice@example.com',now())")
	fpr, _, err := ReadFingerprint(ctx, pool, ReadOptions{Database: "p"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if fpr.Labels != nil {
		t.Fatalf("labels stored without opt-in: %v", fpr.Labels)
	}
	raw, err := json.Marshal(fpr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leaked := range []string{"secret", "alice", "email", "customer"} {
		if strings.Contains(strings.ToLower(string(raw)), leaked) {
			t.Fatalf("fingerprint leaks %q: %s", leaked, raw)
		}
	}
	named, _, err := ReadFingerprint(ctx, pool, ReadOptions{Database: "p",
		IncludeNames: true})
	if err != nil {
		t.Fatalf("read named: %v", err)
	}
	if len(named.Labels) != 2 {
		t.Fatalf("opt-in labels = %v, want 2 tables", named.Labels)
	}
	for _, label := range named.Labels {
		if !strings.HasPrefix(label, "public.secret_") {
			t.Fatalf("label = %q", label)
		}
	}
}

func TestReadFingerprint_QueryShapesFromStatements(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "fp_queries")
	var hasPGSS bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname='pg_stat_statements')`).Scan(&hasPGSS); err != nil {
		t.Fatalf("check pgss: %v", err)
	}
	tenantSchema(t, pool, "orders", "customers")
	for i := 0; i < 3; i++ {
		exec(t, pool, "SELECT count(*) FROM orders WHERE customer_id = $1", i)
	}
	fpr, _, err := ReadFingerprint(ctx, pool, ReadOptions{Database: "q"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := QueryShapeHash("SELECT count(*) FROM orders WHERE customer_id = $1")
	found := false
	for _, q := range fpr.Queries {
		found = found || q == want
	}
	if hasPGSS && !found {
		t.Fatalf("query shape %s missing from %v", want, fpr.Queries)
	}
	if !hasPGSS && len(fpr.Queries) != 0 {
		t.Fatalf("queries without pg_stat_statements: %v", fpr.Queries)
	}
}

func TestReadFingerprint_EmptyDatabaseAndCancelledContext(t *testing.T) {
	pool := freshDB(t, "fp_empty")
	fpr, idx, err := ReadFingerprint(context.Background(), pool, ReadOptions{Database: "e"})
	if err != nil {
		t.Fatalf("empty database: %v", err)
	}
	if len(fpr.Tables) != 0 || len(idx) != 0 {
		t.Fatalf("empty database has shapes: %+v %v", fpr, idx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ReadFingerprint(ctx, pool, ReadOptions{}); err == nil ||
		!strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("cancelled read error = %v, want a fingerprint error", err)
	}
	if _, _, err := ReadFingerprint(context.Background(), nil, ReadOptions{}); err == nil {
		t.Fatal("nil pool must fail")
	}
}

func TestReadFingerprint_MaxTablesBoundsTheRead(t *testing.T) {
	pool := freshDB(t, "fp_max")
	for i := 0; i < 5; i++ {
		exec(t, pool, "CREATE TABLE t"+string(rune('0'+i))+" (a int, b text)")
	}
	fpr, idx, err := ReadFingerprint(context.Background(), pool,
		ReadOptions{MaxTables: 3})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(idx) != 3 {
		t.Fatalf("read %d tables, want the 3-table bound", len(idx))
	}
	if len(fpr.Tables) != 1 {
		t.Fatalf("identical shapes must dedupe to 1 hash, got %v", fpr.Tables)
	}
}

func TestReadTableShape(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "fp_table")
	tenantSchema(t, pool, "orders", "customers")
	_, idx, err := ReadFingerprint(ctx, pool, ReadOptions{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, name := range []string{"orders", "public.orders", "PUBLIC.ORDERS"} {
		got, err := ReadTableShape(ctx, pool, name)
		if err != nil || got != idx["public.orders"] {
			t.Fatalf("ReadTableShape(%q) = %q,%v want %q", name, got, err,
				idx["public.orders"])
		}
	}
	if got, err := ReadTableShape(ctx, pool, "public.nope"); err != nil || got != "" {
		t.Fatalf("missing table = %q,%v want empty, nil", got, err)
	}
	if got, err := ReadTableShape(ctx, pool, "'; DROP TABLE orders; --"); err != nil ||
		got != "" {
		t.Fatalf("hostile name = %q,%v want empty, nil", got, err)
	}
	if got, err := ReadTableShape(ctx, pool, ""); err != nil || got != "" {
		t.Fatalf("empty name = %q,%v", got, err)
	}
}

func TestReadOutcomeDigest_GroupsByClassAndTableShape(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "fp_digest")
	tenantSchema(t, pool, "orders", "customers")
	seedOutcome(t, pool, "index_create", "public.orders", "improved")
	seedOutcome(t, pool, "index_create", "public.orders", "improved")
	seedOutcome(t, pool, "index_create", "public.orders", "regressed")
	seedOutcome(t, pool, "index_create", "public.customers", "neutral")
	seedOutcome(t, pool, "index_create", "public.gone", "improved")
	seedOutcome(t, pool, "index_create", "public.orders", "pending")
	seedOutcome(t, pool, "index_create", "public.orders", "insufficient_evidence")
	_, idx, err := ReadFingerprint(ctx, pool, ReadOptions{})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	counts, err := ReadOutcomeDigest(ctx, pool, idx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	byKey := map[string]OutcomeCount{}
	for _, c := range counts {
		byKey[c.Class+"/"+c.Shape] = c
	}
	orders := byKey["index_create/"+idx["public.orders"]]
	if orders.Improved != 2 || orders.Regressed != 1 || orders.Neutral != 0 {
		t.Fatalf("orders shape = %+v, want 2 improved 1 regressed", orders)
	}
	all := byKey["index_create/"]
	if all.Improved != 3 || all.Regressed != 1 || all.Neutral != 1 {
		t.Fatalf("class total = %+v, want 3/1/1 (unknown table counts here only)", all)
	}
	future, err := ReadOutcomeDigest(ctx, pool, idx, time.Now().Add(time.Hour))
	if err != nil || len(future) != 0 {
		t.Fatalf("since the future = %+v,%v want none", future, err)
	}
	if _, err := ReadOutcomeDigest(ctx, nil, idx, time.Time{}); err == nil {
		t.Fatal("nil pool must fail")
	}
}
