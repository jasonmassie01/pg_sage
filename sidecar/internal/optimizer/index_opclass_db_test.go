package optimizer

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// indexes-01 on real PostgreSQL: the default operator class and collation
// PostgreSQL applies to a key (and pg_get_indexdef omits) are read from
// the catalog, so an explicit default still reads as a duplicate while a
// pattern-ops index beside a plain btree is admitted and measured.

const opclassSchema = "opclass_probe"

// opclassFixture creates opclass_probe.people with a plain btree on email
// and returns the snapshot the optimizer would hold for it.
func opclassFixture(t *testing.T, pool *pgxpool.Pool, extra ...string) *collector.Snapshot {
	t.Helper()
	ctx := context.Background()
	setup := []string{
		"DROP SCHEMA IF EXISTS " + opclassSchema + " CASCADE",
		"CREATE SCHEMA " + opclassSchema,
		"CREATE TYPE " + opclassSchema + ".mood AS ENUM ('ok', 'meh')",
		"CREATE DOMAIN " + opclassSchema + ".label AS text",
		`CREATE TABLE ` + opclassSchema + `.people (id int PRIMARY KEY, email text,
			vc varchar(40), code text COLLATE "C", doc jsonb, tags int[],
			mood ` + opclassSchema + `.mood, lbl ` + opclassSchema + `.label)`,
		`INSERT INTO ` + opclassSchema + `.people (id, email, vc, code)
			SELECT g, md5(g::text) || '@example.com', md5(g::text), md5(g::text)
			FROM generate_series(1, 50000) g`,
		"CREATE INDEX people_email ON " + opclassSchema + ".people (email)",
	}
	for _, sql := range append(setup, extra...) {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("fixture %q: %v", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, "ANALYZE "+opclassSchema+".people"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+opclassSchema+
			" CASCADE")
	})
	return opclassSnapshot(t, pool)
}

func opclassSnapshot(t *testing.T, pool *pgxpool.Pool) *collector.Snapshot {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT c.relname,
		pg_get_indexdef(i.indexrelid), i.indisvalid, i.indisunique
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = 'opclass_probe.people'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	defer rows.Close()
	snap := &collector.Snapshot{Tables: []collector.TableStats{{SchemaName: opclassSchema,
		RelName: "people", NLiveTup: 50000, SeqScan: 10}}}
	for rows.Next() {
		ix := collector.IndexStats{SchemaName: opclassSchema, RelName: "people"}
		if err := rows.Scan(&ix.IndexRelName, &ix.IndexDef, &ix.IsValid,
			&ix.IsUnique); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		snap.Indexes = append(snap.Indexes, ix)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	return snap
}

func opclassDBContext(t *testing.T, o *Optimizer, snap *collector.Snapshot) TableContext {
	t.Helper()
	tc, ok, err := o.TableContext(context.Background(), snap, opclassSchema+".people")
	if err != nil || !ok {
		t.Fatalf("table context: ok=%v err=%v", ok, err)
	}
	return tc
}

func TestFetchKeyDefaults_ReadsTheCatalog(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	opclassFixture(t, pool)
	got, err := fetchKeyDefaults(context.Background(), pool, opclassSchema, "people")
	if err != nil {
		t.Fatalf("fetchKeyDefaults: %v", err)
	}
	check := func(col, method string, want []string) {
		t.Helper()
		if g := got[col].Opclass[method]; !reflect.DeepEqual(g, want) {
			t.Errorf("%s %s default opclass = %v, want %v", col, method, g, want)
		}
	}
	check("email", "btree", []string{"text_ops"})
	check("email", "hash", []string{"text_ops"})
	check("vc", "btree", []string{"text_ops"})  // binary compatible with text
	check("lbl", "btree", []string{"text_ops"}) // a domain uses its base type
	check("doc", "gin", []string{"jsonb_ops"})
	check("tags", "gin", []string{"array_ops"})
	check("mood", "btree", []string{"enum_ops"})
	check("id", "btree", []string{"int4_ops"})
	check("doc", "hnsw", nil)
	for col, want := range map[string][]string{"email": {`"default"`}, "code": {`"C"`},
		"id": nil} {
		if g := got[col].Collation; !reflect.DeepEqual(g, want) {
			t.Errorf("%s collation = %v, want %v", col, g, want)
		}
	}
	if _, ok := got["missing"]; ok || len(got) != 8 {
		t.Errorf("columns = %d (%v), want the table's 8", len(got), got)
	}
}

// Error propagation: a canceled context is an error, not an empty map
// that would read as "no defaults".
func TestFetchKeyDefaults_CanceledIsAnError(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := fetchKeyDefaults(ctx, pool, opclassSchema, "people"); err == nil ||
		got != nil {
		t.Fatalf("canceled = %v %v, want an error and no defaults", got, err)
	}
}

// CHECK-03: the full admission (validator, then the HypoPG what-if) admits
// text_pattern_ops beside btree (email) and measures the LIKE prefix gain
// a plain btree cannot give under a non-C collation.
func TestAdmit_PatternOpsBesidePlainBtreeIsMeasured(t *testing.T) {
	pool := hypopgSessionPool(t)
	var collate string
	if err := pool.QueryRow(context.Background(), `SELECT datcollate FROM pg_database
		WHERE datname = current_database()`).Scan(&collate); err != nil {
		t.Fatal(err)
	}
	if collate == "C" || collate == "POSIX" {
		t.Skipf("database collation %s: a plain btree already serves LIKE prefixes", collate)
	}
	snap := opclassFixture(t, pool)
	o := New(pool, &config.OptimizerConfig{HypoPGMinImprovePct: 10, MaxIndexesPerTable: 10,
		MaxNewPerTable: 3}, noopLog2)
	tc := opclassDBContext(t, o, snap)
	tc.Queries = []QueryInfo{{QueryID: 1, Calls: 500, TotalTimeMs: 5000, MeanTimeMs: 10,
		Text: "SELECT id FROM opclass_probe.people WHERE email LIKE 'abc%'"}}
	adm := o.Admit(context.Background(), Recommendation{IndexType: "btree",
		DDL: "CREATE INDEX CONCURRENTLY people_email_pattern ON opclass_probe.people " +
			"(email text_pattern_ops)"}, tc)
	if adm.Outcome != AdmitAccepted || adm.Rec.WhatIf != WhatIfVerified ||
		adm.Rec.EstimatedImprovementPct <= 10 {
		t.Fatalf("admission = %s (%s) what-if %s %.1f%%", adm.Outcome, adm.Reason,
			adm.Rec.WhatIf, adm.Rec.EstimatedImprovementPct)
	}
	// The same candidate without the opclass is the existing index.
	adm = o.Admit(context.Background(), Recommendation{IndexType: "btree",
		DDL: "CREATE INDEX CONCURRENTLY people_email2 ON opclass_probe.people (email)"}, tc)
	if adm.Outcome != AdmitInvalid || !strings.Contains(adm.Reason, "people_email") {
		t.Fatalf("plain duplicate = %s (%s), want invalid naming people_email",
			adm.Outcome, adm.Reason)
	}
	if !slices.Contains(tc.KeyDefaults["email"].Opclass["btree"], "text_ops") {
		t.Fatalf("table context carries no key defaults: %v", tc.KeyDefaults)
	}
}
