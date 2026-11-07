package migration

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

// No integration test: UnprovenStatements reads only the SQL text; the
// lint and agentguard callers are exercised against PostgreSQL in their
// own packages.

func TestUnprovenStatementsAllowlist(t *testing.T) {
	proven := []string{
		"CREATE INDEX CONCURRENTLY i ON app.t (c)",
		"CREATE TABLE app.t (id int)",
		"CREATE OR REPLACE VIEW app.v AS SELECT 1",
		"CREATE SCHEMA app2",
		"COMMENT ON COLUMN app.t.c IS 'x'",
		"COMMENT ON CONSTRAINT k ON app.t IS 'x'",
		"CREATE SEQUENCE app.s",
		"ALTER TABLE app.t ADD COLUMN c int",
		"ALTER TABLE app.t ADD COLUMN c character varying(20) COLLATE \"C\"",
		"ALTER TABLE app.t ADD COLUMN c double precision DEFAULT 1.5",
		"ALTER TABLE app.t ADD COLUMN c pg_catalog.int8 DEFAULT -1",
		"ALTER TABLE app.t ADD COLUMN c timestamp(3) without time zone DEFAULT now()",
		"ALTER TABLE \"App\".\"T\" ADD COLUMN \"Mixed, Case\" jsonb",
		"SET lock_timeout TO '2s'",
		"SET SESSION statement_timeout = 0",
		"START TRANSACTION",
		"COMMIT WORK",
	}
	for _, sql := range proven {
		got, err := UnprovenStatements(sql)
		if err != nil || len(got) != 0 {
			t.Errorf("UnprovenStatements(%q) = %q, %v; want none, nil", sql, got, err)
		}
	}
}

// The result lists the statements outside the allowlist, in order, in
// the sanitized and collapsed form DDLClassification.Statement uses.
func TestUnprovenStatementsReturnsTheStatements(t *testing.T) {
	sql := "SET lock_timeout = '1s';\nALTER TABLE app.t\n  SET LOGGED;\n" +
		"CREATE TABLE app.n (id int); -- note\nALTER TABLE app.t RENAME TO u"
	got, err := UnprovenStatements(sql)
	if err != nil {
		t.Fatalf("UnprovenStatements: %v", err)
	}
	want := []string{"ALTER TABLE app.t SET LOGGED", "ALTER TABLE app.t RENAME TO u"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnprovenStatements = %q, want %q", got, want)
	}
	byRule := map[string]bool{}
	for _, c := range NewRegexClassifier().Classify("CREATE INDEX i ON app.t\n (c)", 140000) {
		byRule[c.Statement] = true
	}
	stmts, err := UnprovenStatements("CREATE INDEX i ON app.t\n (c)")
	if err != nil || len(stmts) != 1 || !byRule[stmts[0]] {
		t.Fatalf("statement %q (err %v) is not the classifier's form %v", stmts, err, byRule)
	}
}

func TestUnprovenStatementsKeywordFloor(t *testing.T) {
	cases := []string{
		"CREATE TABLE app.t (id int PRIMARY KEY)",
		"CREATE TABLE app.t (id int NOT NULL)",
		"CREATE TABLE app.t (id int CHECK (id > 0))",
		"CREATE TABLE app.t (id int UNIQUE)",
		"CREATE TABLE app.t (r int4range, EXCLUDE USING gist (r WITH &&))",
		"CREATE VIEW app.v AS SELECT * FROM app.t WHERE c IS NOT NULL",
		"ALTER TABLE app.t ADD COLUMN c int COMPRESSION lz4",
		"ALTER TABLE app.t ADD COLUMN c interval day to second",
		"ALTER TABLE app.t ADD COLUMN \"c\" \"text\"",
	}
	for _, sql := range cases {
		got, err := UnprovenStatements(sql)
		if err != nil || len(got) != 1 {
			t.Errorf("UnprovenStatements(%q) = %q, %v; want the statement", sql, got, err)
		}
	}
}

func TestUnprovenStatementsEmpty(t *testing.T) {
	for _, sql := range []string{"", "  \n", "-- only a comment", "/* c */ ;;"} {
		got, err := UnprovenStatements(sql)
		if err != nil || got != nil {
			t.Errorf("UnprovenStatements(%q) = %#v, %v; want nil, nil", sql, got, err)
		}
	}
}

func TestUnprovenStatementsUntokenizable(t *testing.T) {
	cases := []string{
		"CREATE TABLE app.t (a text DEFAULT 'x); ALTER TABLE app.big SET LOGGED",
		"CREATE TABLE app.t (a text DEFAULT 'x'')",
		`CREATE TABLE app.t (a text DEFAULT E'x\')`,
		"CREATE TABLE app.t (a text DEFAULT $tag$x$ta)",
		`CREATE TABLE app."t (id int)`,
		"CREATE TABLE app.t (id int) /* outer /* inner */",
		"'",
	}
	for _, sql := range cases {
		got, err := UnprovenStatements(sql)
		if !errors.Is(err, ErrUntokenizable) || got != nil {
			t.Errorf("UnprovenStatements(%q) = %q, %v; want nil, ErrUntokenizable",
				sql, got, err)
		}
	}
	closed := []string{
		"CREATE TABLE app.t (a text DEFAULT 'it''s')",
		`CREATE TABLE app.t (a text DEFAULT E'x\'y')`,
		"CREATE TABLE app.t (a text DEFAULT $tag$x$tag$)",
		"CREATE TABLE app.t (a text) /* outer /* inner */ */",
		"CREATE TABLE app.t (a text) -- trailing comment without newline",
	}
	for _, sql := range closed {
		if _, err := UnprovenStatements(sql); err != nil {
			t.Errorf("UnprovenStatements(%q) error %v, want it tokenized", sql, err)
		}
	}
}

// Sanitizing keeps its output for unterminated input: the rest of the
// text is one redacted literal, exactly as before the scanner reported
// termination.
func TestSanitizeDDLUnterminatedUnchanged(t *testing.T) {
	cases := map[string]string{
		"SELECT 'abc":          "SELECT '***'",
		"SELECT $$abc":         "SELECT '***'",
		"SELECT 1 /* x":        "SELECT 1",
		`SELECT "abc`:          `SELECT "abc`,
		"SELECT 'a' -- b\n, 2": "SELECT '***'  \n, 2",
	}
	for in, want := range cases {
		if got := sanitizeDDL(in); got != want {
			t.Errorf("sanitizeDDL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnprovenStatementsConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	fails := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := UnprovenStatements("ALTER TABLE t SET LOGGED; CREATE TABLE u (id int)")
			if err != nil || len(got) != 1 || got[0] != "ALTER TABLE t SET LOGGED" {
				fails <- "unexpected result"
			}
		}()
	}
	wg.Wait()
	close(fails)
	for f := range fails {
		t.Error(f)
	}
}

// PostgreSQL 16+ treats a vertical tab as whitespace. The checks must
// read it as a space, while the returned statement keeps the classifier's
// form so lint can match it against rule hits.
func TestUnprovenStatementsVerticalTab(t *testing.T) {
	got, err := UnprovenStatements("CREATE TABLE app.c AS\v(SELECT 1)")
	want := []string{"CREATE TABLE app.c AS\v(SELECT 1)"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("UnprovenStatements = %q, %v; want %q", got, err, want)
	}
	for _, sql := range []string{
		"ALTER\vTABLE app.t ADD\vCOLUMN c\vint",
		"COMMENT\vON TABLE t IS 'x'",
	} {
		if got, err := UnprovenStatements(sql); err != nil || len(got) != 0 {
			t.Errorf("UnprovenStatements(%q) = %q, %v; want none", sql, got, err)
		}
	}
}
