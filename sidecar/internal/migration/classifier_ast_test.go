//go:build cgo

package migration

import (
	"context"
	"sync"
	"testing"
)

func astClassify(sql string, version int) Classification {
	return classifyText(sql, version, astStatement)
}

// wantOnPG is the rule a missed form gets on a major; "" means the form
// is a syntax error there, so it must be unrecognized.
func wantOnPG(rule string, major int) string {
	switch {
	case rule == "ddl_set_access_method" && major < 15:
		return ""
	case rule == "ddl_not_null_constraint_scan" && major < 18:
		return ""
	}
	return rule
}

// CHECK-05, parser half: every missed form gets its rule on PG 14-18
// (SET ACCESS METHOD from 15, the NOT NULL constraint from 18) and is a
// known statement; where the server rejects the form it is unrecognized.
func TestASTClassifierCatchesMissedFormsOnEveryMajor(t *testing.T) {
	for major := 14; major <= 18; major++ {
		for _, f := range missedForms {
			st := astClassify(f.sql, major*10000).Statements[0]
			want := wantOnPG(f.rule, major)
			ids := hazardRules(st.Classifications)
			if want == "" {
				if st.Status != StatementUnrecognized || len(ids) != 0 {
					t.Fatalf("PG %d %q: %v %s, want unrecognized", major, f.sql, ids, st.Status)
				}
				continue
			}
			if st.Status != StatementKnown || len(ids) != 1 || ids[0] != want {
				t.Fatalf("PG %d %q: rules %v status %s, want [%s] known",
					major, f.sql, ids, st.Status, want)
			}
		}
	}
}

// The table is named on every form that has one.
func TestASTClassifierNamesTheTable(t *testing.T) {
	for _, f := range missedForms {
		c := astClassify(f.sql, 180000).Statements[0].Classifications[0]
		if c.SchemaName != "app" || (c.TableName != "t" && c.TableName != "p") {
			t.Fatalf("%q: target %s.%s, want app.t or app.p", f.sql, c.SchemaName, c.TableName)
		}
	}
}

// Boundary: the safe spellings of the same forms are known and clear.
func TestASTClassifierSafeSpellings(t *testing.T) {
	for _, tc := range []struct {
		sql   string
		major int
	}{
		{"ALTER TABLE app.p DETACH PARTITION app.p1 CONCURRENTLY", 14},
		{"ALTER TABLE app.p DETACH PARTITION app.p1 FINALIZE", 14},
		{"ALTER TABLE app.t ADD CONSTRAINT c_nn NOT NULL c NOT VALID", 18},
		{"ALTER TABLE app.t ADD CONSTRAINT ck CHECK (v > 0) NOT VALID", 14},
		{"ALTER TABLE app.t ADD CONSTRAINT fk FOREIGN KEY (a) REFERENCES app.p (id) NOT VALID", 14},
		{"ALTER TABLE app.t VALIDATE CONSTRAINT ck", 14},
		{"ALTER TABLE app.t ADD COLUMN c int NOT NULL DEFAULT 0", 14},
		{"CREATE INDEX CONCURRENTLY i ON app.t (c) WHERE c IS NOT NULL", 14},
		{"VACUUM (VERBOSE) app.t", 14},
		{"ANALYZE app.t", 14},
		{"REINDEX TABLE CONCURRENTLY app.t", 14},
		{"SET search_path = app", 14},
	} {
		st := astClassify(tc.sql, tc.major*10000).Statements[0]
		if st.Status != StatementKnown || len(st.Classifications) != 0 {
			t.Fatalf("%q on PG %d: %v %s, want known and clear",
				tc.sql, tc.major, ruleIDs(st.Classifications), st.Status)
		}
	}
}

// Invalid input: a parse error, a DML statement, a drop and an unmapped
// DDL are never known; a parse error keeps the regex rule it matched.
func TestASTClassifierUnrecognized(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE app.t FROBNICATE", "CREATE TABLE app.t (id int", "SELECT 1",
		"GRANT SELECT ON app.t TO reader", "ALTER TABLE app.t ADD COLUMN c app.positive_int",
		"CREATE TABLE app.p1 PARTITION OF app.p FOR VALUES IN (1)",
		"ALTER TABLE app.t ALTER COLUMN c SET EXPRESSION AS (a * 2)", "TRUNCATE app.t",
		"DROP INDEX CONCURRENTLY app.i", "DROP VIEW app.v",
	} {
		if st := astClassify(sql, 170000).Statements[0]; st.Status != StatementUnrecognized {
			t.Fatalf("%q: status %s, want unrecognized", sql, st.Status)
		}
	}
	st := astClassify("ALTER TABLE t ALTER COLUMN a SET NOT NULL FROBNICATE", 170000).Statements[0]
	if st.Status != StatementUnrecognized || len(hazardRules(st.Classifications)) != 1 {
		t.Fatalf("parse error lost the regex floor: %+v", st)
	}
}

// State: the statement's strongest lock wins. ADD COLUMN ... REFERENCES
// holds ACCESS EXCLUSIVE, so the FK rule reports it and the batch is told
// to set lock_timeout.
func TestASTClassifierStrongestLockWins(t *testing.T) {
	cs := astClassify("ALTER TABLE app.t ADD COLUMN c int REFERENCES app.p(id)", 170000).All()
	if len(cs) != 2 || cs[0].RuleID != "ddl_fk_not_valid" ||
		cs[0].LockLevel != lockAccessExclusive ||
		cs[1].RuleID != "ddl_missing_lock_timeout" {
		t.Fatalf("classifications %+v", cs)
	}
	cs = astClassify("ALTER TABLE app.t ADD CONSTRAINT fk FOREIGN KEY (a) REFERENCES app.p (id)",
		170000).All()
	if len(cs) != 1 || cs[0].LockLevel != lockShareRowExclusive {
		t.Fatalf("a plain ADD FOREIGN KEY: %+v, want SHARE ROW EXCLUSIVE and no annotation", cs)
	}
}

// The production classifier is the parse-tree one on a cgo build.
func TestClassifyStatementsUsesParserOnCgo(t *testing.T) {
	res, err := ClassifyStatements(context.Background(), nil, "ALTER TABLE t SET LOGGED", 0)
	if err != nil || res.Degraded || len(hazardRules(res.All())) != 1 {
		t.Fatalf("cgo build: %+v %v, want the parser's ddl_set_logged", res, err)
	}
	if got := hazardRules(NewClassifier().Classify("ALTER TABLE t SET LOGGED", 0)); len(got) != 1 {
		t.Fatalf("NewClassifier: %v", got)
	}
}

func TestASTStatementConcurrentCallers(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, f := range missedForms {
				ids := hazardRules(astClassify(f.sql, 180000).All())
				if len(ids) != 1 || ids[0] != f.rule {
					errs <- f.sql
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("diverging classification of %q", e)
	}
}
