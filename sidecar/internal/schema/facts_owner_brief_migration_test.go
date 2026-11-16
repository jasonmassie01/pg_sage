package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// evid-03a: sage.facts' fact_type and subject_kind checks gain owner_brief,
// do_not_touch (evid-03b) and the database subject kind. The step finds
// the checks to replace in pg_constraint (single-column CHECKs on the
// column), so it works whatever an earlier step named them; it keeps every
// row and a second bootstrap changes nothing.
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestFactsOwnerBriefStepIsLastAndBounded(t *testing.T) {
	steps := trancheFiveMigrations()
	// evid-02c's ddlWorkloadPseudonymKey was added after this step; it must
	// stay in tranche five, right after the workload packs.
	idx := -1
	for i, step := range steps {
		if step[0] == "ddlFactsOwnerBrief" {
			idx = i
		}
	}
	if idx < 1 || steps[idx-1][0] != "ddlWorkloadPacks" {
		t.Fatalf("T5 steps = %v, want ddlFactsOwnerBrief after ddlWorkloadPacks", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout", "pg_catalog.pg_constraint",
		"facts_fact_type_v2", "facts_subject_kind_v2", "'owner_brief'", "'do_not_touch'",
		"'database'", "'column_class'", "'column'"} {
		if !strings.Contains(ddlFactsOwnerBrief, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlFactsOwnerBrief) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// factChecks lists the single-column CHECK constraints on col of
// sage.facts as name=oid.
func factChecks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, col string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT c.conname || '=' || c.oid::text
		FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid
		WHERE c.conrelid = 'sage.facts'::regclass AND c.contype = 'c'
		  AND a.attname = $1 AND c.conkey = ARRAY[a.attnum]
		ORDER BY 1`, col)
	if err != nil {
		t.Fatalf("read %s checks: %v", col, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// restorePreStepChecks puts back the checks as ddlAgentEnvClass left
// them, so the step runs against a database that predates it.
func restorePreStepChecks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE sage.facts DROP CONSTRAINT IF EXISTS facts_fact_type_v2`,
		`ALTER TABLE sage.facts DROP CONSTRAINT IF EXISTS facts_subject_kind_v2`,
		`ALTER TABLE sage.facts ADD CONSTRAINT facts_fact_type_v2 CHECK (fact_type IN (
			'owned_by_app_migrations', 'test_fixture', 'slot_consumer', 'append_only',
			'table_window', 'column_class'))`,
		`ALTER TABLE sage.facts ADD CONSTRAINT facts_subject_kind_v2 CHECK (subject_kind IN (
			'index', 'table', 'schema', 'slot', 'column'))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func TestFactsOwnerBriefMigrationKeepsRowsAndIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.facts
			WHERE subject LIKE 'evid03a%' OR fact_type IN ('owner_brief', 'do_not_touch')`)
	}
	clean()
	t.Cleanup(clean)
	restorePreStepChecks(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		value, source) VALUES
		('owned_by_app_migrations', 'table', 'evid03a.t', '{}', 'operator'),
		('test_fixture', 'schema', 'evid03a_s', '{}', 'detector'),
		('slot_consumer', 'slot', 'evid03a_slot', '{"consumer":"x"}', 'operator'),
		('append_only', 'table', 'evid03a.log', '{}', 'model'),
		('table_window', 'table', 'evid03a.w', '{"kind":"batch","window":"always"}',
		 'operator')`); err != nil {
		t.Fatalf("seed existing facts: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		source) VALUES ('owner_brief', 'database', '*', 'operator')`); err == nil {
		t.Fatal("the pre-step checks accepted an owner brief; the test setup is wrong")
	}
	bootstrapWithRetry(t, ctx, pool)
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.facts
		WHERE subject LIKE 'evid03a%'`).Scan(&kept); err != nil || kept != 5 {
		t.Fatalf("%d of 5 existing facts kept (%v)", kept, err)
	}
	types, kinds := factChecks(t, ctx, pool, "fact_type"), factChecks(t, ctx, pool,
		"subject_kind")
	if len(types) != 1 || !strings.HasPrefix(types[0], "facts_fact_type_v2=") ||
		len(kinds) != 1 || !strings.HasPrefix(kinds[0], "facts_subject_kind_v2=") {
		t.Fatalf("checks after the step: %v %v, want only the widened v2 ones", types, kinds)
	}
	assertFactsOwnerBriefChecks(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	again := append(factChecks(t, ctx, pool, "fact_type"),
		factChecks(t, ctx, pool, "subject_kind")...)
	if strings.Join(again, ",") != strings.Join(append(types, kinds...), ",") {
		t.Fatalf("a second bootstrap rewrote the checks: %v, was %v %v", again, types, kinds)
	}
}

func assertFactsOwnerBriefChecks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	insert := `INSERT INTO sage.facts (fact_type, subject_kind, subject, source)
		VALUES ($1, $2, $3, 'operator')`
	for _, ok := range [][3]string{{"owner_brief", "database", "*"},
		{"do_not_touch", "table", "evid03a.keep"}} {
		if _, err := pool.Exec(ctx, insert, ok[0], ok[1], ok[2]); err != nil {
			t.Errorf("%s/%s refused: %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][3]string{{"nonsense", "table", "evid03a.x"},
		{"append_only", "cluster", "evid03a.y"}} {
		_, err := pool.Exec(ctx, insert, bad[0], bad[1], bad[2])
		if err == nil || !strings.Contains(err.Error(), "check") {
			t.Errorf("%s/%s accepted: %v", bad[0], bad[1], err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		source, subject_relid, subject_attnum) VALUES ('column_class', 'column',
		'evid03a.16400.2', 'operator', 16400, 2)`); err != nil {
		t.Errorf("column class fact refused after the swap: %v", err)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM sage.facts WHERE subject = 'evid03a.16400.2'`)
}

// An untrusted boot (the ledger's catalog fingerprint gone: a boot cut
// off, an operator's schema change) reruns every step, ddlAgentEnvClass
// among them. It must neither put the narrower lists back nor fail on an
// owner brief row already stored.
func TestFactsOwnerBriefSurvivesAnUntrustedBoot(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.facts
			WHERE subject LIKE 'evid03a%' OR fact_type IN ('owner_brief', 'do_not_touch')`)
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		value, source) VALUES ('owner_brief', 'database', '*', '{"owner":"ops"}',
		'operator')`); err != nil {
		t.Fatalf("store an owner brief: %v", err)
	}
	before := append(factChecks(t, ctx, pool, "fact_type"),
		factChecks(t, ctx, pool, "subject_kind")...)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.schema_migrations
		WHERE id = $1`, ledgerCatalogID); err != nil {
		t.Fatalf("drop the catalog fingerprint: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("untrusted boot with an owner brief stored: %v", err)
	}
	after := append(factChecks(t, ctx, pool, "fact_type"),
		factChecks(t, ctx, pool, "subject_kind")...)
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("the untrusted boot changed the checks: %v, was %v", after, before)
	}
	var briefs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.facts
		WHERE fact_type = 'owner_brief' AND value->>'owner' = 'ops'`).Scan(&briefs); err != nil ||
		briefs != 1 {
		t.Fatalf("%d owner briefs kept, want 1 (%v)", briefs, err)
	}
	clean()
	assertFactsOwnerBriefChecks(t, ctx, pool)
}
