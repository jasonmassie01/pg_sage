package explain

import (
	"errors"
	"strings"
	"testing"
)

// ReadOnlyStatement (sre-07) is ReadStatement that also refuses a read-
// looking statement that writes or locks rows: a data-modifying CTE, FOR
// UPDATE/SHARE and SELECT INTO. PREPARE alone takes the statement's locks
// (RowExclusiveLock for a DELETE CTE), so these never reach the server.
// The cases hold in both builds: the parse tree with cgo, the token scan
// without it.

// No concurrent access tests: ReadOnlyStatement is a pure function of its
// argument and holds no shared state.

func TestReadOnlyStatement_AllowsReadStatements(t *testing.T) {
	cases := []string{
		"SELECT v FROM t WHERE id = $1",
		"SELECT update_count, deleted_at FROM t;",
		"WITH r AS (SELECT 1 AS x) SELECT x FROM r",
		`SELECT "update", 'DELETE FROM t' FROM t`,
		"/* INSERT INTO t */ SELECT 1",
		"-- FOR UPDATE\nSELECT 1",
		"SELECT $tag$ MERGE INTO t $tag$",
		"VALUES (1), (2)",
		`SELECT E'it''s \' FOR SHARE' AS s`,
		"SELECT inserted, updated_by, merged FROM events",
	}
	for _, in := range cases {
		want := strings.TrimSuffix(in, ";")
		got, err := ReadOnlyStatement(in)
		if err != nil || got != want {
			t.Errorf("ReadOnlyStatement(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
}

func TestReadOnlyStatement_RefusesWritesAndRowLocks(t *testing.T) {
	refused := []string{
		"WITH d AS (DELETE FROM t WHERE id = $1 RETURNING id) SELECT id FROM d",
		"WITH u AS (UPDATE t SET v = $1 RETURNING id) SELECT count(*) FROM u",
		"WITH i AS (INSERT INTO t (id) VALUES ($1) RETURNING id) SELECT * FROM i",
		"with d as (delete from t returning id) select id from d",
		"SELECT v FROM t WHERE id = $1 FOR UPDATE",
		"SELECT v FROM t FOR NO KEY UPDATE",
		"SELECT v FROM t FOR SHARE",
		"SELECT v FROM t FOR KEY SHARE SKIP LOCKED",
		"SELECT v INTO new_table FROM t",
		"(WITH d AS (DELETE FROM t RETURNING id) SELECT id FROM d)",
	}
	for _, in := range refused {
		got, err := ReadOnlyStatement(in)
		if !errors.Is(err, ErrExplainInvalidRequest) || got != "" {
			t.Errorf("ReadOnlyStatement(%q) = %q, %v; want ErrExplainInvalidRequest", in,
				got, err)
		}
	}
}

func TestReadOnlyStatement_KeepsReadStatementRefusals(t *testing.T) {
	for _, in := range []string{"", "   ", "DELETE FROM t", "SELECT 1; SELECT 2",
		"SELECT 'unterminated", "UPDATE t SET v = 1", "/* open"} {
		if got, err := ReadOnlyStatement(in); !errors.Is(err, ErrExplainInvalidRequest) ||
			got != "" {
			t.Errorf("ReadOnlyStatement(%q) = %q, %v; want ErrExplainInvalidRequest", in,
				got, err)
		}
	}
}

func TestReadOnlyStatement_ReadStatementStillAcceptsDMLCTEs(t *testing.T) {
	// The /explain API's validator (explainBody) is unchanged: only the
	// SRE investigator's EXPLAIN uses the stricter check.
	in := "WITH d AS (DELETE FROM t RETURNING id) SELECT id FROM d"
	if got, err := ReadStatement(in); err != nil || got != in {
		t.Fatalf("ReadStatement(%q) = %q, %v; want it unchanged", in, got, err)
	}
}
