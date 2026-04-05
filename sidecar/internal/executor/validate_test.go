package executor

import (
	"errors"
	"testing"
)

func TestValidateSQL_AllowedStatements(t *testing.T) {
	allowed := []string{
		"CREATE INDEX CONCURRENTLY idx ON t(id)",
		"CREATE UNIQUE INDEX CONCURRENTLY idx ON t(id)",
		"DROP INDEX CONCURRENTLY IF EXISTS idx",
		"DROP INDEX idx",
		"REINDEX INDEX CONCURRENTLY idx",
		"VACUUM ANALYZE public.users",
		"VACUUM",
		"ANALYZE public.orders",
		"ALTER TABLE public.t SET (fillfactor = 90)",
		"ALTER SYSTEM SET work_mem = '256MB'",
		"ALTER SYSTEM RESET work_mem",
		"SET lock_timeout = '5s'",
		"RESET lock_timeout",
		"SELECT pg_terminate_backend(12345)",
		"SELECT pg_cancel_backend(12345)",
		"  CREATE INDEX idx ON t(id)  ",
		"create index concurrently idx on t(id)",
	}
	for _, sql := range allowed {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf(
				"expected allowed: %q, got error: %v",
				sql, err,
			)
		}
	}
}

func TestValidateSQL_RejectedStatements(t *testing.T) {
	rejected := []string{
		"DROP TABLE users",
		"DELETE FROM users WHERE 1=1",
		"INSERT INTO users VALUES (1)",
		"UPDATE users SET admin = true",
		"ALTER ROLE postgres SUPERUSER",
		"TRUNCATE TABLE users",
		"GRANT ALL ON users TO evil",
		"CREATE TABLE evil (id int)",
		"COPY users TO '/tmp/dump'",
		// SELECT restricted to specific functions
		"SELECT 1",
		"SELECT * FROM users",
		// ALTER SYSTEM restricted to whitelisted GUCs
		"ALTER SYSTEM SET wal_level = 'logical'",
		"ALTER SYSTEM SET listen_addresses = '*'",
		"ALTER SYSTEM RESET all",
		// ALTER TABLE restricted to safe sub-commands
		"ALTER TABLE public.t DROP COLUMN name",
		"ALTER TABLE public.t RENAME TO new_name",
		"ALTER TABLE public.t OWNER TO evil",
		"ALTER TABLE public.t ADD COLUMN evil int",
		"ALTER TABLE public.t ENABLE TRIGGER ALL",
	}
	for _, sql := range rejected {
		err := ValidateExecutorSQL(sql)
		if err == nil {
			t.Errorf("expected rejected: %q", sql)
			continue
		}
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf(
				"expected ErrDisallowedSQL for %q, got: %v",
				sql, err,
			)
		}
	}
}

func TestValidateSQL_MultiStatement(t *testing.T) {
	cases := []string{
		"CREATE INDEX idx ON t(id); DROP TABLE users",
		"VACUUM; DELETE FROM users",
		"SELECT 1; SELECT 2",
	}
	for _, sql := range cases {
		err := ValidateExecutorSQL(sql)
		if err == nil {
			t.Errorf("expected rejected (multi): %q", sql)
			continue
		}
		if !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf(
				"expected ErrDisallowedSQL for %q, got: %v",
				sql, err,
			)
		}
	}
}

func TestValidateSQL_CaseInsensitive(t *testing.T) {
	cases := []string{
		"create index concurrently idx on t(id)",
		"Vacuum Analyze public.t",
		"ALTER system SET work_mem = '64MB'",
		"REINDEX index idx",
		"set lock_timeout = '5s'",
	}
	for _, sql := range cases {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf(
				"expected allowed (case insensitive): %q, got: %v",
				sql, err,
			)
		}
	}
}

func TestValidateSQL_EmptyString(t *testing.T) {
	err := ValidateExecutorSQL("")
	if err == nil {
		t.Error("expected error for empty SQL")
	}
	if !errors.Is(err, ErrDisallowedSQL) {
		t.Errorf("expected ErrDisallowedSQL, got: %v", err)
	}
}

func TestValidateSQL_TrailingSemicolon(t *testing.T) {
	// Trailing semicolon with only whitespace after = OK.
	sql := "CREATE INDEX idx ON t(id);"
	if err := ValidateExecutorSQL(sql); err != nil {
		t.Errorf("trailing semicolon should be ok: %v", err)
	}
}

func TestValidateSQL_AlterDatabase(t *testing.T) {
	cases := []string{
		`ALTER DATABASE "mydb" SET work_mem = '64MB'`,
		`ALTER DATABASE "mydb" RESET work_mem`,
		`ALTER DATABASE postgres SET max_wal_size = '4GB'`,
		`alter database "mydb" set work_mem = '64MB'`,
	}
	for _, sql := range cases {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("expected ALTER DATABASE allowed: %q, got: %v",
				sql, err)
		}
	}
}
