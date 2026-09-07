package executor

import "testing"

func TestValidateExecutorSQLProtectsSageSchema(t *testing.T) {
	tests := []string{
		"CREATE INDEX CONCURRENTLY idx ON sage.action_log (id)",
		"DROP INDEX CONCURRENTLY sage.idx_action_log_time",
		"ANALYZE sage.action_log",
		"VACUUM sage.action_log",
		`CREATE INDEX CONCURRENTLY idx ON "SAGE".action_log (id)`,
	}
	for _, statement := range tests {
		t.Run(statement, func(t *testing.T) {
			if err := ValidateExecutorSQL(statement); err == nil {
				t.Fatalf("ValidateExecutorSQL accepted protected schema: %s", statement)
			}
		})
	}
}
