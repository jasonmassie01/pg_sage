package lint

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Regression tests for schema-lint bugs recorded in docs/reviews/2026-09-26.

// fakeRows feeds fixed rows into a rule's collect() method.
type fakeRows struct {
	rows [][]any
	i    int
}

func (f *fakeRows) Next() bool { f.i++; return f.i <= len(f.rows) }
func (f *fakeRows) Err() error { return nil }
func (f *fakeRows) Scan(dest ...any) error {
	row := f.rows[f.i-1]
	if len(row) != len(dest) {
		return fmt.Errorf("scan: %d values for %d dests", len(row), len(dest))
	}
	for i, v := range row {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

// G4-B23/C16: every DDL-building lint rule must quote identifiers.
func TestRegression_LintDDLQuotesIdentifiers(t *testing.T) {
	type collector interface {
		collect(interface {
			Next() bool
			Scan(...any) error
			Err() error
		}) ([]Finding, error)
	}
	cases := []struct {
		name string
		rule collector
		row  []any
		want string
	}{
		{"overlapping", &ruleOverlappingIndex{},
			[]any{"Sales", "Orders", "IdxA", "IdxAB", int64(8192)},
			`DROP INDEX CONCURRENTLY "Sales"."IdxA";`},
		{"char", &ruleCharUsage{}, []any{"Sales", "Orders", "Code", 3},
			`ALTER TABLE "Sales"."Orders" ALTER COLUMN "Code" TYPE text;`},
		{"varchar", &ruleVarchar255{}, []any{"Sales", "Orders", "Name"},
			`ALTER TABLE "Sales"."Orders" ALTER COLUMN "Name" TYPE text;`},
		{"timestamp", &ruleTimestampNoTZ{}, []any{"Sales", "Orders", "At"},
			`ALTER TABLE "Sales"."Orders" ALTER COLUMN "At" TYPE timestamptz ` +
				`USING "At" AT TIME ZONE 'UTC';`},
		{"int_pk", &ruleIntPK{}, []any{"Sales", "Orders", "Id", int64(2_000_000)},
			`ALTER TABLE "Sales"."Orders" ALTER COLUMN "Id" TYPE bigint;`},
		{"fk_type", &ruleFKTypeMismatch{},
			[]any{"Sales", "Orders", "CustId", "integer",
				"Sales", "Customers", "Id", "bigint"},
			`ALTER TABLE "Sales"."Orders" ALTER COLUMN "CustId" TYPE bigint;`},
		{"txid", &ruleTxidAge{}, []any{"Sales", "Orders", int64(9e8), int64(10)},
			`VACUUM FREEZE "Sales"."Orders";`},
		{"mxid", &ruleMxidAge{}, []any{"Sales", "Orders", int64(9e8), int64(10)},
			`VACUUM FREEZE "Sales"."Orders";`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ff, err := tc.rule.collect(&fakeRows{rows: [][]any{tc.row}})
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if len(ff) != 1 || ff[0].SQL != tc.want {
				t.Fatalf("SQL = %q, want %q", sqlOf(ff), tc.want)
			}
		})
	}
}

func sqlOf(ff []Finding) string {
	if len(ff) == 0 {
		return "<no finding>"
	}
	return ff[0].SQL
}

// G2-B27: exclude_schemas entries with uppercase/hyphens/spaces must be
// honoured (safely quoted), not silently dropped.
func TestRegression_ExcludeSchemasAcceptsQuotedNames(t *testing.T) {
	got := schemaExcludeSQL([]string{"Sales", "my-schema", "o'brien"})
	for _, want := range []string{`'Sales'`, `'my-schema'`, `'o''brien'`} {
		if !strings.Contains(got, want) {
			t.Fatalf("exclude list %s lacks %s", got, want)
		}
	}
}

// G2-B10: the overlapping-index rule must never recommend dropping a
// primary-key, unique or constraint-backing index.
func TestRegression_OverlappingIndexSkipsUniqueAndPK(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	ddl := fmt.Sprintf(`
		CREATE TABLE %[1]s.ov (id int PRIMARY KEY, code int, created_at int);
		CREATE INDEX ov_id_created ON %[1]s.ov (id, created_at);
		CREATE UNIQUE INDEX ov_code_uq ON %[1]s.ov (code);
		CREATE INDEX ov_code_created ON %[1]s.ov (code, created_at)`, schema)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	ff, err := (&ruleOverlappingIndex{}).Check(ctx, pool, defaultOpts(schema))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, f := range ff {
		if f.Schema == schema {
			t.Fatalf("unique/PK prefix reported for drop: %s (%s)", f.Index, f.SQL)
		}
	}
}

type failingRule struct{ id string }

func (r *failingRule) ID() string       { return r.id }
func (r *failingRule) Name() string     { return "failing" }
func (r *failingRule) Severity() string { return "info" }
func (r *failingRule) Category() string { return "convention" }
func (r *failingRule) Check(
	context.Context, *pgxpool.Pool, RuleOpts,
) ([]Finding, error) {
	return nil, errors.New("transient failure")
}

// G2-B14: a transiently failing rule must not resolve its open findings.
func TestRegression_FailingRuleDoesNotResolveFindings(t *testing.T) {
	pool, ctx := requireDB(t)
	serializeAcrossPackages(t, ctx, pool)
	dbName := fmt.Sprintf("test_db_%d", time.Now().UnixNano())
	schema := createSchema(t, pool, ctx)
	t.Cleanup(func() { cleanLintFindings(pool, dbName) })

	cfg := &config.SchemaLintConfig{Enabled: true, MinTableRows: -1}
	runner := NewRunner(pool, cfg, 160000, dbName, testLogFn(t))
	if err := runner.upsertFindings(ctx, []Finding{{
		RuleID: "lint_flaky", Schema: schema, Table: "tbl",
		Severity: "info", Category: "convention",
		Description: "desc", Impact: "impact", Suggestion: "sug",
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runner.linter.rules = []Rule{&failingRule{id: "lint_flaky"}}
	runner.scan(ctx)
	if got := lintFindingCount(t, pool, ctx, "lint_flaky", dbName, "open"); got != 1 {
		t.Fatalf("open findings after failed rule = %d, want 1", got)
	}
}
