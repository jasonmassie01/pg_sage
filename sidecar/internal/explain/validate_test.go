package explain

import (
	"context"
	"errors"
	"testing"
)

// G6-B01: /explain accepts exactly one read-only statement. The
// allowlist replaces the old leading-keyword DDL denylist.

func TestValidateExplainQuery_AllowsSingleReadStatement(t *testing.T) {
	allowed := []string{
		"SELECT 1",
		"select * from t where id = $1",
		"  \n\tSELECT 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"VALUES (1), (2)",
		"TABLE pg_class",
		"(SELECT 1) UNION (SELECT 2)",
		"/* leading comment */ SELECT 1",
		"/* nested /* comment */ still */ SELECT 1",
		"-- line comment\nSELECT 1",
		"SELECT ';' AS semi",
		"SELECT 'it''s; fine'",
		"SELECT E'esc\\'; still literal'",
		`SELECT "odd;name" FROM t`,
		"SELECT $$ ; $$",
		"SELECT $tag$ ; DROP TABLE t; $tag$",
		"SELECT 1 -- trailing ; comment",
		"SELECT 1;",
		"SELECT 1 ;  \n",
		"SELECT 1; -- trailing comment only",
		"SELECT $1::int + $2",
	}
	for _, q := range allowed {
		if err := validateExplainQuery(q); err != nil {
			t.Errorf("validateExplainQuery(%q) = %v, want nil", q, err)
		}
	}
}

func TestValidateExplainQuery_RejectsMultipleStatements(t *testing.T) {
	rejected := []string{
		"SELECT 1; SELECT 2",
		"SELECT 1;COMMIT",
		"SELECT $1; COMMIT; DELETE FROM t",
		"SELECT 1; /* c */ DROP TABLE t",
		"SELECT 'a'; DELETE FROM t",
		"SELECT $$x$$; DELETE FROM t",
		"SELECT 1;;",
		"SELECT E'\\\\'; DELETE FROM t",
	}
	for _, q := range rejected {
		err := validateExplainQuery(q)
		if !errors.Is(err, ErrExplainInvalidRequest) {
			t.Errorf("validateExplainQuery(%q) = %v, want %v",
				q, err, ErrExplainInvalidRequest)
		}
	}
}

func TestValidateExplainQuery_RejectsNonReadStatements(t *testing.T) {
	rejected := []string{
		"", "   ", "-- only a comment",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"MERGE INTO t USING s ON true WHEN MATCHED THEN DELETE",
		"CREATE TABLE t (id int)", "DROP TABLE t", "ALTER TABLE t ADD c int",
		"TRUNCATE t", "GRANT ALL ON t TO public", "REVOKE ALL ON t FROM x",
		"COPY t TO STDOUT", "CLUSTER t", "REINDEX TABLE t",
		"BEGIN", "COMMIT", "ROLLBACK", "SET ROLE postgres",
		"RESET ALL", "DO $$ BEGIN END $$", "CALL p()",
		"EXPLAIN SELECT 1", "PREPARE p AS SELECT 1", "EXECUTE p",
		"LISTEN x", "VACUUM t", "SELECTX 1", "WITHOUT 1",
	}
	for _, q := range rejected {
		err := validateExplainQuery(q)
		if !errors.Is(err, ErrExplainInvalidRequest) {
			t.Errorf("validateExplainQuery(%q) = %v, want %v",
				q, err, ErrExplainInvalidRequest)
		}
	}
}

func TestValidateExplainQuery_RejectsUnterminatedTokens(t *testing.T) {
	rejected := []string{
		"SELECT 'unterminated",
		"SELECT 1 /* unterminated",
		`SELECT "unterminated`,
		"SELECT $x$ unterminated",
		"SELECT E'unterminated\\'",
	}
	for _, q := range rejected {
		err := validateExplainQuery(q)
		if !errors.Is(err, ErrExplainInvalidRequest) {
			t.Errorf("validateExplainQuery(%q) = %v, want %v",
				q, err, ErrExplainInvalidRequest)
		}
	}
}

// Explain must reject a multi-statement body before touching the
// pool. A nil pool proves no connection was attempted: reaching the
// database would panic.
func TestExplain_RejectsMultiStatementBeforeDB(t *testing.T) {
	ex := New(nil, nil, noopLogFn)
	for _, q := range []string{
		"SELECT $1; COMMIT; DELETE FROM t",
		"SELECT 1; COMMIT; DELETE FROM t",
		"DELETE FROM t",
	} {
		res, err := ex.Explain(context.Background(),
			ExplainRequest{Query: q})
		if !errors.Is(err, ErrExplainInvalidRequest) {
			t.Errorf("Explain(%q) err = %v, want %v",
				q, err, ErrExplainInvalidRequest)
		}
		if res != nil {
			t.Errorf("Explain(%q) result = %+v, want nil", q, res)
		}
	}
}
