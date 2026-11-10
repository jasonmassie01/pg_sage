package migration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// missedForms are the lock or rewrite forms the regex rules miss
// (migration-04). The parse-tree classifier gives each a rule on PG 18;
// the regex path must at least report each as unrecognized.
var missedForms = []struct{ sql, rule string }{
	{"ALTER TABLE app.t SET LOGGED", "ddl_set_logged"},
	{"ALTER TABLE app.t SET UNLOGGED", "ddl_set_logged"},
	{"ALTER TABLE app.t SET ACCESS METHOD heap2", "ddl_set_access_method"},
	{"ALTER TABLE app.t ADD CONSTRAINT ex EXCLUDE USING gist (r WITH &&)",
		"ddl_add_exclusion_builds_index"},
	{"ALTER TABLE app.p DETACH PARTITION app.p1", "ddl_detach_partition_not_concurrent"},
	{"ALTER TABLE app.t ADD CONSTRAINT c_nn NOT NULL c", "ddl_not_null_constraint_scan"},
	{"ALTER TABLE app.t RENAME TO t2", "ddl_rename"},
	{"ALTER TABLE app.t RENAME COLUMN a TO b", "ddl_rename"},
	{"CREATE TRIGGER trg BEFORE INSERT ON app.t FOR EACH ROW EXECUTE FUNCTION app.f()",
		"ddl_create_trigger"},
	{"ALTER TABLE app.t ADD COLUMN c text UNIQUE", "ddl_add_key_builds_index"},
	{"ALTER TABLE app.t ADD COLUMN c int REFERENCES app.p(id)", "ddl_fk_not_valid"},
	{"ALTER TABLE app.t ADD COLUMN c int CHECK (c > 0)", "ddl_constraint_not_valid"},
}

func hazardRules(cs []DDLClassification) []string {
	var ids []string
	for _, c := range cs {
		if c.RuleID != "ddl_missing_lock_timeout" {
			ids = append(ids, c.RuleID)
		}
	}
	return ids
}

// CHECK-05, regex half: the regex rules recognize none of the 12 forms,
// so the degraded path reports each as unrecognized, never known safe.
func TestRegexPathMissesFormsButFailsClosed(t *testing.T) {
	if len(missedForms) != 12 {
		t.Fatalf("the plan names 12 missed forms, table has %d", len(missedForms))
	}
	for _, f := range missedForms {
		res := classifyText(f.sql, 180000, regexStatement)
		if len(res.Statements) != 1 {
			t.Fatalf("%q: %d statements", f.sql, len(res.Statements))
		}
		st := res.Statements[0]
		if ids := hazardRules(st.Classifications); len(ids) != 0 {
			t.Fatalf("%q: regex rules %v; the table no longer pins a miss", f.sql, ids)
		}
		if st.Status != StatementUnrecognized {
			t.Fatalf("%q: regex path status %s, want unrecognized", f.sql, st.Status)
		}
	}
}

// On whichever classifier this build has, no missed form is known safe:
// it either matches a hazard rule or is unrecognized.
func TestClassifyStatementsNeverClearsMissedForms(t *testing.T) {
	for _, f := range missedForms {
		res, err := ClassifyStatements(context.Background(), nil, f.sql, 180000)
		if err != nil {
			t.Fatalf("%q: %v", f.sql, err)
		}
		st := res.Statements[0]
		if st.Status == StatementKnown && len(hazardRules(st.Classifications)) == 0 {
			t.Fatalf("%q classified known safe: %+v", f.sql, st)
		}
	}
}

func TestClassifyStatementsBatchShape(t *testing.T) {
	sql := "SET lock_timeout = '5s'; ALTER TABLE t ALTER COLUMN a SET NOT NULL; SELECT 1"
	res, err := ClassifyStatements(context.Background(), nil, sql, 170000)
	if err != nil || res.Untokenizable || len(res.Statements) != 3 {
		t.Fatalf("result %+v err %v, want 3 tokenized statements", res, err)
	}
	if res.Statements[0].Status != StatementKnown || len(res.Statements[0].Classifications) != 0 {
		t.Fatalf("SET lock_timeout: %+v", res.Statements[0])
	}
	ids := ruleIDs(res.Statements[1].Classifications)
	if len(ids) != 1 || ids[0] != "ddl_set_not_null" {
		t.Fatalf("SET NOT NULL after lock_timeout: rules %v, want only ddl_set_not_null", ids)
	}
	if res.Statements[1].Status != StatementKnown ||
		res.Statements[2].Status != StatementUnrecognized {
		t.Fatalf("statuses %s, %s; want known, unrecognized",
			res.Statements[1].Status, res.Statements[2].Status)
	}
	if res.Unrecognized() != 1 || len(res.All()) != 1 || res.Degraded == parserAvailable {
		t.Fatalf("Unrecognized %d, All %v, Degraded %v", res.Unrecognized(), ruleIDs(res.All()),
			res.Degraded)
	}
}

// Nil/empty and invalid input: no statements for empty text; text that
// ends inside a literal is flagged untokenizable.
func TestClassifyStatementsEmptyAndUntokenizable(t *testing.T) {
	for _, sql := range []string{"", "   ", ";;", "-- only a comment"} {
		res, err := ClassifyStatements(context.Background(), nil, sql, 0)
		if err != nil || len(res.Statements) != 0 || res.Unrecognized() != 0 || res.All() != nil {
			t.Fatalf("%q: %+v %v, want no statements", sql, res, err)
		}
	}
	res, _ := ClassifyStatements(context.Background(), nil,
		"CREATE TABLE t (a text DEFAULT 'x); ALTER TABLE big SET LOGGED", 0)
	if !res.Untokenizable {
		t.Fatalf("unterminated literal not flagged: %+v", res)
	}
}

// fakeQuerier answers the column type lookup with typ or err.
type fakeQuerier struct {
	typ   string
	err   error
	calls int
	mu    sync.Mutex
}

func (f *fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeQuerier: Query not supported")
}

func (f *fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return fakeRow{typ: f.typ, err: f.err}
}

type fakeRow struct {
	typ string
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.typ
	return nil
}

func alterTypeRule(t *testing.T, res Classification) DDLClassification {
	t.Helper()
	for _, c := range res.All() {
		if c.RuleID == "ddl_alter_type_rewrite" {
			return c
		}
	}
	t.Fatalf("no ddl_alter_type_rewrite in %v", ruleIDs(res.All()))
	return DDLClassification{}
}

// The binary-coercible check runs inside the one entry point, so every
// caller (lint included) sees varchar(20) -> text as no rewrite.
func TestClassifyStatementsRefinesAlterType(t *testing.T) {
	sql := "ALTER TABLE app.t ALTER COLUMN v TYPE text"
	q := &fakeQuerier{typ: "character varying(20)"}
	res, err := ClassifyStatements(context.Background(), q, sql, 170000)
	if err != nil || q.calls != 1 {
		t.Fatalf("err %v, %d lookups", err, q.calls)
	}
	if c := alterTypeRule(t, res); c.RequiresRewrite ||
		!strings.Contains(c.Description, "Binary-coercible") {
		t.Fatalf("varchar(20) -> text still a rewrite: %+v", c)
	}
	res, _ = ClassifyStatements(context.Background(), &fakeQuerier{typ: "integer"},
		"ALTER TABLE app.t ALTER COLUMN v TYPE bigint", 170000)
	if !alterTypeRule(t, res).RequiresRewrite {
		t.Fatal("integer -> bigint marked no rewrite")
	}
	res, _ = ClassifyStatements(context.Background(), nil, sql, 170000)
	if !alterTypeRule(t, res).RequiresRewrite {
		t.Fatal("without a catalog the change must stay a rewrite")
	}
}

// Error propagation: a failed lookup is returned with the column named,
// the classification is complete and the change stays a rewrite; a column
// that does not exist yet is no error.
func TestClassifyStatementsRefineErrors(t *testing.T) {
	sql := "ALTER TABLE app.t ALTER COLUMN v TYPE text"
	boom := errors.New("canceling statement due to statement timeout")
	res, err := ClassifyStatements(context.Background(), &fakeQuerier{err: boom}, sql, 0)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "app.t.v") {
		t.Fatalf("err = %v, want the timeout naming app.t.v", err)
	}
	if !alterTypeRule(t, res).RequiresRewrite {
		t.Fatal("a failed lookup lowered the rewrite")
	}
	res, err = ClassifyStatements(context.Background(), &fakeQuerier{err: pgx.ErrNoRows}, sql, 0)
	if err != nil || !alterTypeRule(t, res).RequiresRewrite {
		t.Fatalf("missing column: err %v, rewrite %v", err, alterTypeRule(t, res).RequiresRewrite)
	}
}

// Concurrent access: parallel callers share the rule set and the
// degraded-log once; every answer matches the serial one.
func TestClassifyStatementsConcurrentCallers(t *testing.T) {
	want, _ := ClassifyStatements(context.Background(), nil, missedForms[0].sql, 0)
	var wg sync.WaitGroup
	errs := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _ := ClassifyStatements(context.Background(), nil, missedForms[0].sql, 0)
			if got.Statements[0].Status != want.Statements[0].Status ||
				len(got.All()) != len(want.All()) {
				errs <- "diverging result"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// A USING expression can compute any value: even varchar(20) -> text
// stays a rewrite, and no catalog read is spent on it.
func TestClassifyStatementsAlterTypeUsingStaysRewrite(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE app.t ALTER COLUMN v TYPE text USING lower(v)",
		`ALTER TABLE app.t ALTER COLUMN v TYPE text COLLATE "C" USING upper(v)`,
		"ALTER TABLE app.t ALTER COLUMN v SET DATA TYPE varchar USING v || 'x'",
	} {
		q := &fakeQuerier{typ: "character varying(20)"}
		res, err := ClassifyStatements(context.Background(), q, sql, 170000)
		if err != nil || q.calls != 0 {
			t.Fatalf("%q: err %v, %d lookups, want none", sql, err, q.calls)
		}
		if c := alterTypeRule(t, res); !c.RequiresRewrite || c.ColumnName != "v" {
			t.Fatalf("%q: %+v, want a rewrite of v", sql, c)
		}
	}
}

// hiddenTypeRewrites are ALTER COLUMN TYPE spellings whose rewrite (or
// index rebuild) the one target type of a classification cannot show: a
// COLLATE clause, or a second type change in the same statement.
var hiddenTypeRewrites = []string{
	"ALTER TABLE app.t ALTER COLUMN v TYPE text, ALTER COLUMN n TYPE bigint",
	"ALTER TABLE app.t ALTER COLUMN v TYPE text, ALTER COLUMN n TYPE bigint USING n::bigint",
	`ALTER TABLE app.t ALTER COLUMN v TYPE text COLLATE "C"`,
	`ALTER TABLE app.t ALTER v SET DATA TYPE varchar(30) COLLATE "C", ALTER n SET DEFAULT 0`,
}

// On either classifier (the build's, and the regex path a cgo build also
// keeps), these stay a rewrite of v and read no column type: the varchar
// column v alone would be binary coercible (migration-04 review).
func TestClassifyStatementsHiddenTypeRewritesStayRewrite(t *testing.T) {
	for _, sql := range hiddenTypeRewrites {
		q := &fakeQuerier{typ: "character varying(20)"}
		res, err := ClassifyStatements(context.Background(), q, sql, 170000)
		if err != nil || q.calls != 0 {
			t.Fatalf("%q: err %v, %d lookups, want none", sql, err, q.calls)
		}
		if c := alterTypeRule(t, res); !c.RequiresRewrite || c.ColumnName != "v" {
			t.Fatalf("%q: %+v, want a rewrite of v", sql, c)
		}
		if c := alterTypeRule(t, classifyText(sql, 170000, regexStatement)); c.TargetType != "" ||
			c.ColumnName != "v" {
			t.Fatalf("regex path %q: %+v, want no target type", sql, c)
		}
	}
	// A current column with a collation other than the default comes back
	// with it (the lookup's contract), and is never binary coercible.
	q := &fakeQuerier{typ: `character varying(20) COLLATE "C"`}
	res, err := ClassifyStatements(context.Background(), q,
		"ALTER TABLE app.t ALTER COLUMN v TYPE text", 170000)
	if err != nil || q.calls != 1 || !alterTypeRule(t, res).RequiresRewrite {
		t.Fatalf("varchar COLLATE \"C\" -> text: %+v err %v calls %d, want a rewrite",
			alterTypeRule(t, res), err, q.calls)
	}
}
