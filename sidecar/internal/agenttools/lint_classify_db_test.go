package agenttools

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: LintMigration keeps no state between calls;
// ClassifyStatements' own concurrency test covers the shared rule set.

// requireRuleOrUnrecognized: with the SQL parser (cgo) the statement must
// match rule; without it, the note must say the DDL is unrecognized.
func requireRuleOrUnrecognized(t *testing.T, res LintResult, rule string) {
	t.Helper()
	for _, it := range res.Statements {
		if it.RuleID == rule {
			return
		}
	}
	require.False(t, sqlast.Available(), "parser build: rule %s missing from %+v", rule,
		res.Statements)
	require.Contains(t, res.Note, "unrecognized DDL is not proven safe")
}

// Integration: lint and the advisor read the same column type, so they
// agree that varchar(20) -> text keeps the rows (no rewrite) and that
// integer -> bigint rewrites them. Before migration-04 lint skipped the
// binary-coercible check and reported the varchar change as a rewrite.
func TestLintAndAdvisorAgreeOnColumnTypeChanges(t *testing.T) {
	f := newFixture(t)
	table := f.q("ty")
	f.exec("CREATE TABLE "+table+" (v varchar(20), n int)",
		"INSERT INTO "+table+" SELECT i::text, i FROM generate_series(1, 20000) i",
		"ANALYZE "+table)
	cfg := &config.MigrationConfig{Enabled: true, DDLRowThreshold: 1}
	advisor := migration.NewAdvisor(f.pool, cfg, 0, "db", func(string, string, ...any) {}, nil)
	for sql, rewrite := range map[string]bool{
		"ALTER TABLE " + table + " ALTER COLUMN v TYPE text":   false,
		"ALTER TABLE " + table + " ALTER COLUMN n TYPE bigint": true,
	} {
		res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: sql})
		require.NoError(t, err)
		var item *LintItem
		for i := range res.Statements {
			if res.Statements[i].RuleID == "ddl_alter_type_rewrite" {
				item = &res.Statements[i]
			}
		}
		require.NotNil(t, item, "lint %q: %+v", sql, res.Statements)
		require.Equal(t, rewrite, item.RequiresRewrite, "lint rewrite for %q", sql)
		_, risk, err := advisor.AnalyzeRisk(f.ctx, sql)
		require.NoError(t, err)
		require.NotNil(t, risk, "advisor reported nothing for %q", sql)
		require.Equal(t, rewrite, risk.RequiresRewrite, "advisor rewrite for %q", sql)
		require.Equal(t, item.RequiresRewrite, risk.RequiresRewrite, "lint and advisor disagree")
	}
	var typ string
	f.scalar(&typ, "SELECT format_type(atttypid, atttypmod) FROM pg_attribute "+
		"WHERE attrelid = $1::regclass AND attname = 'v'", table)
	require.Equal(t, "character varying(20)", typ, "linting changed the column")
}

// The 12 lock or rewrite forms the regex rules missed are never safe on a
// real server, and with the parser each names its rule.
func TestLintMigrationMissedFormsAreNeverSafe(t *testing.T) {
	f := newFixture(t)
	t1, p, p1 := f.q("t"), f.q("p"), f.q("p1")
	f.exec("CREATE TABLE "+t1+" (id int PRIMARY KEY, a int, c int, r int4range)",
		"CREATE TABLE "+p+" (id int PRIMARY KEY) PARTITION BY RANGE (id)",
		"CREATE TABLE "+p1+" PARTITION OF "+p+" FOR VALUES FROM (0) TO (10)",
		"CREATE FUNCTION "+f.q("f")+"() RETURNS trigger LANGUAGE plpgsql AS "+
			"$$BEGIN RETURN NEW; END$$")
	at := "ALTER TABLE " + t1
	forms := []struct{ sql, rule string }{
		{at + " SET LOGGED", "ddl_set_logged"},
		{at + " SET UNLOGGED", "ddl_set_logged"},
		{at + " SET ACCESS METHOD heap", "ddl_set_access_method"},
		{at + " ADD CONSTRAINT ex EXCLUDE USING gist (r WITH &&)",
			"ddl_add_exclusion_builds_index"},
		{"ALTER TABLE " + p + " DETACH PARTITION " + p1, "ddl_detach_partition_not_concurrent"},
		{at + " RENAME TO t2", "ddl_rename"},
		{at + " RENAME COLUMN a TO b", "ddl_rename"},
		{"CREATE TRIGGER trg BEFORE INSERT ON " + t1 + " FOR EACH ROW EXECUTE FUNCTION " +
			f.q("f") + "()", "ddl_create_trigger"},
		{at + " ADD COLUMN u text UNIQUE", "ddl_add_key_builds_index"},
		{at + " ADD COLUMN x int REFERENCES " + p + "(id)", "ddl_fk_not_valid"},
		{at + " ADD COLUMN k int CHECK (k > 0)", "ddl_constraint_not_valid"},
	}
	for _, form := range forms {
		res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: form.sql,
			PGVersion: 17})
		require.NoError(t, err)
		require.NotEqual(t, "safe", res.Verdict, "SQL %q", form.sql)
		requireRuleOrUnrecognized(t, res, form.rule)
	}
	// PG 18's NOT NULL constraint is a syntax error before 18: unrecognized.
	res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{
		SQL: at + " ADD CONSTRAINT c_nn NOT NULL c", PGVersion: 17})
	require.NoError(t, err)
	require.Equal(t, "review", res.Verdict)
	require.True(t, strings.Contains(res.Note, "unrecognized"), "note %q", res.Note)
	require.True(t, f.relationExists(t1), "linting renamed the table")
}

// relfilenodes maps the table and each of its indexes, by name, to its
// file node, which a rewrite or an index rebuild changes.
func (f *fixture) relfilenodes(q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, table string) map[string]int64 {
	f.t.Helper()
	rows, err := q.Query(f.ctx, `SELECT c.relname, c.relfilenode::bigint FROM pg_class c
		WHERE c.oid = $1::regclass OR c.oid IN
		  (SELECT indexrelid FROM pg_index WHERE indrelid = $1::regclass)`, table)
	require.NoError(f.t, err)
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var node int64
		require.NoError(f.t, rows.Scan(&name, &node))
		out[name] = node
	}
	require.NoError(f.t, rows.Err())
	return out
}

// Integration (migration-04 review): a second type change in the same
// statement, a COLLATE clause, and a column whose collation ALTER TYPE
// resets each rewrite the table or rebuild an index on a real server.
// Lint and the advisor must both say rewrite, although v alone
// (varchar(20) -> text) is binary coercible.
func TestLintAndAdvisorKeepHiddenTypeRewrites(t *testing.T) {
	f := newFixture(t)
	table := f.q("hid")
	f.exec("CREATE TABLE "+table+` (v varchar(20), n int, w varchar(20) COLLATE "C")`,
		"CREATE INDEX hid_v ON "+table+" (v)", "CREATE INDEX hid_w ON "+table+" (w)",
		"INSERT INTO "+table+" SELECT i::text, i, i::text FROM generate_series(1, 2000) i")
	cfg := &config.MigrationConfig{Enabled: true, DDLRowThreshold: 1}
	advisor := migration.NewAdvisor(f.pool, cfg, 0, "db", func(string, string, ...any) {}, nil)
	at := "ALTER TABLE " + table + " ALTER COLUMN "
	for _, sql := range []string{
		at + "v TYPE text, ALTER COLUMN n TYPE bigint",
		at + `v TYPE text COLLATE "C"`,
		at + "w TYPE text",
	} {
		res, err := New(f.pool, Options{}).LintMigration(f.ctx, LintRequest{SQL: sql})
		require.NoError(t, err)
		var item *LintItem
		for i := range res.Statements {
			if res.Statements[i].RuleID == "ddl_alter_type_rewrite" {
				item = &res.Statements[i]
			}
		}
		require.NotNil(t, item, "lint %q: %+v", sql, res.Statements)
		require.True(t, item.RequiresRewrite, "lint called %q no rewrite", sql)
		_, risk, err := advisor.AnalyzeRisk(f.ctx, sql)
		require.NoError(t, err)
		require.NotNil(t, risk, "advisor reported nothing for %q", sql)
		require.True(t, risk.RequiresRewrite, "advisor called %q no rewrite", sql)
		require.True(t, f.rewritesOnServer(table, sql), "server kept every file for %q", sql)
	}
	require.False(t, f.rewritesOnServer(table, at+"v TYPE text"),
		"control: varchar(20) -> text must keep the table and its index")
}

// rewritesOnServer runs sql in a rolled-back transaction and reports
// whether it gave the table or one of its indexes a new file.
func (f *fixture) rewritesOnServer(table, sql string) bool {
	f.t.Helper()
	before := f.relfilenodes(f.pool, table)
	tx, err := f.pool.Begin(f.ctx)
	require.NoError(f.t, err)
	defer func() { _ = tx.Rollback(f.ctx) }()
	_, err = tx.Exec(f.ctx, sql)
	require.NoError(f.t, err, "server rejected %q", sql)
	after := f.relfilenodes(tx, table)
	require.Equal(f.t, len(before), len(after), "relations of %s after %q", table, sql)
	for name, node := range after {
		if before[name] != node {
			return true
		}
	}
	return false
}
