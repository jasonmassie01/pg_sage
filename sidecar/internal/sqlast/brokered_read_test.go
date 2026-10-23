//go:build cgo

package sqlast

import (
	"errors"
	"reflect"
	"testing"
)

// No concurrency tests: InspectBrokeredRead is a pure function of its input
// and shares no state between calls.

func brokered(t *testing.T, sql string) BrokeredRead {
	t.Helper()
	got, err := InspectBrokeredRead(sql)
	if err != nil {
		t.Fatalf("InspectBrokeredRead(%q) error = %v", sql, err)
	}
	return got
}

func TestInspectBrokeredReadCanonicalizes(t *testing.T) {
	got := brokered(t, "/* a comment */ select  id, name FROM app.users -- trailing\n"+
		"WHERE id = $1")
	const want = "SELECT id, name FROM app.users WHERE id = $1"
	if got.Canonical != want {
		t.Errorf("Canonical = %q, want %q", got.Canonical, want)
	}
	if got.Fingerprint == "" {
		t.Error("Fingerprint is empty")
	}
	again := brokered(t, got.Canonical)
	if again.Fingerprint != got.Fingerprint || again.Canonical != got.Canonical {
		t.Errorf("canonical form is not a fixed point: %+v vs %+v", again, got)
	}
	if !hasName(got.Query.Relations, "app", "users") {
		t.Errorf("Relations = %v, want app.users", got.Query.Relations)
	}
}

func TestInspectBrokeredReadFoldsUnicodeEscapes(t *testing.T) {
	got := brokered(t, `SELECT U&"pg\005fsleep"(1)`)
	if got.Canonical != "SELECT pg_sleep(1)" {
		t.Errorf("Canonical = %q, want the escape resolved", got.Canonical)
	}
	if !hasName(got.Query.Functions, "", "pg_sleep") {
		t.Errorf("Functions = %v, want pg_sleep", got.Query.Functions)
	}
}

func TestInspectBrokeredReadRejectsEverythingButOneSelect(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"SELECT 1; SELECT 2",
		"COMMIT; INSERT INTO t VALUES (1)",
		"INSERT INTO t (id) SELECT id FROM s",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"SET statement_timeout = 0",
		"RESET ROLE",
		"SET TRANSACTION READ WRITE",
		"BEGIN",
		"DO $$ BEGIN END $$",
		"CALL p()",
		"COPY t TO STDOUT",
		"LOCK t",
		"LISTEN c",
		"NOTIFY c",
		"PREPARE p AS SELECT 1",
		"EXECUTE p",
		"EXPLAIN ANALYZE SELECT 1",
		"WITH d AS (DELETE FROM t RETURNING id) SELECT * FROM d",
		"SELECT * FROM t FOR UPDATE",
		"SELECT * INTO new_t FROM t",
		"SELECT * FROM t WHERE", // a parse error
	}
	for _, sql := range cases {
		_, err := InspectBrokeredRead(sql)
		if !errors.Is(err, ErrRejected) {
			t.Errorf("InspectBrokeredRead(%q) error = %v, want ErrRejected", sql, err)
		}
	}
}

func TestInspectBrokeredReadAcceptsReadForms(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"VALUES (1), (2)",
		"TABLE app.users",
		"WITH x AS (SELECT 1 AS a) SELECT a FROM x",
		"SELECT a FROM t UNION ALL SELECT b FROM u",
		"SELECT count(*) FROM t WHERE created > now() - interval '1 day'",
	}
	for _, sql := range cases {
		got := brokered(t, sql)
		if got.Canonical == "" {
			t.Errorf("%q: empty canonical form", sql)
		}
	}
}

func TestInspectBrokeredReadSplitsColumnReferences(t *testing.T) {
	cases := []struct {
		sql           string
		output, other []string
	}{
		{"SELECT ssn FROM people", []string{"ssn"}, nil},
		{"SELECT p.ssn, p.id FROM people p", []string{"id", "ssn"}, nil},
		{"SELECT id FROM people WHERE ssn::int = 0", []string{"id"}, []string{"ssn"}},
		{"SELECT ssn::text FROM people", nil, []string{"ssn"}},
		{"SELECT ssn FROM people ORDER BY ssn", []string{"ssn"}, []string{"ssn"}},
		{"SELECT row_to_json(p) FROM people p", nil, []string{"p"}},
		{"SELECT * FROM people", nil, nil},
		{"SELECT ssn FROM people UNION SELECT ssn FROM staff", nil, []string{"ssn"}},
		{"SELECT id FROM people WHERE lower(name) LIKE 'a%'", []string{"id"},
			[]string{"name"}},
		{"SELECT (SELECT ssn FROM people LIMIT 1) AS s", nil, []string{"ssn"}},
		{"SELECT length(ssn) FROM people", nil, []string{"ssn"}},
	}
	for _, c := range cases {
		got := brokered(t, c.sql)
		if !reflect.DeepEqual(got.OutputColumns, c.output) {
			t.Errorf("%q: OutputColumns = %v, want %v", c.sql, got.OutputColumns, c.output)
		}
		if !reflect.DeepEqual(got.OtherColumnRefs, c.other) {
			t.Errorf("%q: OtherColumnRefs = %v, want %v", c.sql, got.OtherColumnRefs,
				c.other)
		}
	}
}

func TestInspectBrokeredReadKeepsParameters(t *testing.T) {
	got := brokered(t, "SELECT id FROM t WHERE a = $1 AND b = $2")
	if got.Canonical != "SELECT id FROM t WHERE a = $1 AND b = $2" {
		t.Errorf("Canonical = %q, want the parameters kept", got.Canonical)
	}
	if got.Params != 2 {
		t.Errorf("Params = %d, want 2", got.Params)
	}
	if brokered(t, "SELECT 1").Params != 0 {
		t.Error("a query without parameters reports parameters")
	}
}
