package schema

import (
	"context"
	"testing"
	"time"
)

// executor-02: sage.verification.observation_failures counts consecutive
// failed observations. Bootstrap adds it once, NOT NULL DEFAULT 0, and a
// second bootstrap on the same database is a no-op.
func TestVerificationObservationFailuresMigrationIsIdempotent(t *testing.T) {
	pool := freshPool(t, "verification_retry")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for run := 1; run <= 2; run++ {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("Bootstrap run %d: %v", run, err)
		}
	}
	var nullable, defaultExpr, dataType string
	err := pool.QueryRow(ctx, `SELECT is_nullable, COALESCE(column_default, ''), data_type
		FROM information_schema.columns
		WHERE table_schema='sage' AND table_name='verification'
		  AND column_name='observation_failures'`).Scan(&nullable, &defaultExpr, &dataType)
	if err != nil {
		t.Fatalf("observation_failures column: %v", err)
	}
	if nullable != "NO" || defaultExpr != "0" || dataType != "integer" {
		t.Fatalf("observation_failures = %s nullable=%s default=%q, want integer NOT NULL 0",
			dataType, nullable, defaultExpr)
	}
	if got := countStatement(migrationStatements(), ddlVerificationObservationRetry); got != 1 {
		t.Fatalf("observation retry migration registered %d times, want once", got)
	}
}

func countStatement(statements []string, want string) int {
	n := 0
	for _, statement := range statements {
		if statement == want {
			n++
		}
	}
	return n
}
