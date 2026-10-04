package facts

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Deterministic detectors propose facts with cited evidence; they never
// confirm anything.

func proposalFor(ps []Proposal, typ Type, subject string) (Proposal, bool) {
	for _, p := range ps {
		if p.Type == typ && p.Subject == subject {
			return p, true
		}
	}
	return Proposal{}, false
}

// An index pg_sage dropped that came back with the same definition (the
// lifeos index fight) is proposed as owned by the app's migrations.
func TestAppManagedDetectorProposesRecreatedIndexes(t *testing.T) {
	pool, ctx := livePool(t)
	table := uniqueName("am_orders_")
	idx := "idx_" + table
	def := fmt.Sprintf("CREATE INDEX %s ON public.%s USING btree (status)", idx, table)
	execAll(t, ctx, pool, fmt.Sprintf("CREATE TABLE public.%s (status text)", table), def)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	for i := 0; i < 3; i++ {
		execAll(t, ctx, pool, fmt.Sprintf(`INSERT INTO sage.action_log (action_type,
			sql_executed, rollback_sql, outcome, executed_at) VALUES ('drop_index',
			'DROP INDEX CONCURRENTLY public.%s', '%s', 'success',
			now() - interval '%d minutes')`, idx, def, 10*(i+1)))
	}
	got, err := NewAppManagedDetector(pool).Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := proposalFor(got, TypeAppMigrations, "public."+idx)
	if !ok || p.Kind != KindIndex || p.Source != SourceDetector || len(p.Evidence) != 1 ||
		!strings.Contains(p.Evidence[0].Detail, "3 times") {
		t.Fatalf("proposals %+v", got)
	}
	if _, err := Validate(p); err != nil {
		t.Fatalf("detector proposal invalid: %v", err)
	}
}

// A family of identically shaped test schemas with no traffic over the
// quiet period is proposed once as a test-fixture pattern; a test schema
// in use, and non-test schemas, are not.
func TestTestFixtureDetectorProposesQuietFamilies(t *testing.T) {
	pool, ctx := livePool(t)
	stem := uniqueName("test_leak") + "_"
	var schemas []string
	for i := 0; i < 3; i++ {
		schemas = append(schemas, fmt.Sprintf("%s%06x", stem, 0xa1b2c0+i))
	}
	active := uniqueName("test_active_")
	other := uniqueName("app_core_")
	for _, s := range append(append([]string(nil), schemas...), active, other) {
		execAll(t, ctx, pool, "CREATE SCHEMA "+s, "CREATE TABLE "+s+".events (id int)")
	}
	t.Cleanup(func() {
		for _, s := range append(append([]string(nil), schemas...), active, other) {
			_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+s+" CASCADE")
		}
	})
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewTestFixtureDetector(pool, time.Hour, clock)
	if got, err := d.Detect(ctx); err != nil || len(got) != 0 {
		t.Fatalf("first observation must not propose: %+v %v", got, err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	insert := "INSERT INTO " + active + ".events SELECT generate_series(1,50)"
	if _, err := conn.Exec(ctx, insert); err != nil {
		t.Fatal(err)
	}
	if err := testdb.FlushStats(ctx, conn); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	got, err := d.Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := proposalFor(got, TypeTestFixture, stem+"*")
	if !ok || p.Kind != KindSchema || len(p.Evidence) == 0 ||
		!strings.Contains(p.Evidence[0].Detail, "3 schemas") {
		t.Fatalf("family proposal missing: %+v", got)
	}
	for _, q := range got {
		if strings.Contains(q.Subject, "test_active_") ||
			strings.Contains(q.Subject, "app_core_") || q.Subject == "sage" {
			t.Fatalf("unexpected proposal %+v", q)
		}
	}
}

func TestSlotConsumerDetectorProposesLogicalSlots(t *testing.T) {
	pool, ctx := livePool(t)
	var level string
	if err := pool.QueryRow(ctx, "SHOW wal_level").Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level != "logical" {
		t.Skipf("wal_level=%s: logical slots need wal_level=logical", level)
	}
	slot := uniqueName("pgsage_cdc_")
	execAll(t, ctx, pool, fmt.Sprintf(
		"SELECT pg_create_logical_replication_slot('%s', 'test_decoding')", slot))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
	})
	got, err := NewSlotConsumerDetector(pool).Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := proposalFor(got, TypeSlotConsumer, slot)
	if !ok || p.Value["consumer"] != "test_decoding" || p.Evidence[0].Ref != "slot:"+slot ||
		!strings.Contains(p.Evidence[0].Detail, "inactive") {
		t.Fatalf("slot proposal: %+v", got)
	}
	if _, err := Validate(p); err != nil {
		t.Fatal(err)
	}
}

func TestAppendOnlyDetectorProposesInsertOnlyTables(t *testing.T) {
	pool, ctx := livePool(t)
	ins, upd := uniqueName("ao_ledger_"), uniqueName("ao_orders_")
	execAll(t, ctx, pool, "CREATE TABLE public."+ins+" (id int)",
		"CREATE TABLE public."+upd+" (id int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+ins+", public."+upd)
	})
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, stmt := range []string{
		"INSERT INTO public." + ins + " SELECT generate_series(1, 2000)",
		"INSERT INTO public." + upd + " SELECT generate_series(1, 2000)",
		"UPDATE public." + upd + " SET id = id + 1 WHERE id < 10",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := testdb.FlushStats(ctx, conn); err != nil {
		t.Fatal(err)
	}
	got, err := NewAppendOnlyDetector(pool, 1000).Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := proposalFor(got, TypeAppendOnly, "public."+ins)
	if !ok || !strings.Contains(p.Evidence[0].Detail, "2000 inserts") {
		t.Fatalf("append-only proposal: %+v", got)
	}
	if _, ok := proposalFor(got, TypeAppendOnly, "public."+upd); ok {
		t.Fatal("a table with updates is not append-only")
	}
}

func TestCollectModelEvidenceIsBoundedAndCitable(t *testing.T) {
	pool, ctx := livePool(t)
	got, err := CollectModelEvidence(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxModelEvidence {
		t.Fatalf("%d evidence items", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		if e.ID == "" || seen[e.ID] || e.Ref == "" || strings.Contains(e.Ref, "sage.") {
			t.Fatalf("evidence %+v", e)
		}
		seen[e.ID] = true
	}
}
