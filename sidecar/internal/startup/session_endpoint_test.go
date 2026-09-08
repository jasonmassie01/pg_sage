package startup

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No shared state or SQL: this rejects known transaction routes before connection
// side effects. Provider live tests exercise actual direct/session connections.
func TestValidateSessionEndpoint(t *testing.T) {
	cases := []struct {
		host    string
		port    uint16
		blocked bool
	}{
		{"ep-test-pooler.us-east-2.aws.neon.tech", 5432, true},
		{"ep-test.us-east-2.aws.neon.tech", 5432, false},
		{"EP-TEST-POOLER.US-EAST-2.AWS.NEON.TECH.", 5432, true},
		{"aws-0-us-east-1.pooler.supabase.com", 6543, true},
		{"aws-0-us-east-1.pooler.supabase.com", 5432, false},
		{"db.project.supabase.co", 5432, false},
		{"db.project.supabase.co", 6543, true},
		{"DB.PROJECT.SUPABASE.CO.", 6543, true},
		{"db.project.supabase.co.attacker.example", 6543, false},
		{"ep-test-pooler.neon.tech.attacker.example", 5432, false},
		{"notpooler.supabase.com", 6543, false},
		{"localhost", 6543, false}, {"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			err := ValidateSessionEndpoint(tc.host, tc.port)
			if (err != nil) != tc.blocked {
				t.Fatalf("error = %v, blocked = %t", err, tc.blocked)
			}
		})
	}
}

func TestRunChecksRejectsHostedTransactionEndpoint(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://unused@aws-0-us-east-1.pooler.supabase.com:6543/test?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	result, err := RunChecks(context.Background(), pool)
	if result != nil || err == nil || !strings.Contains(err.Error(), "session pooler") {
		t.Fatalf("expected endpoint error before connecting, result=%v error=%v", result, err)
	}
	if pool.Stat().TotalConns() != 0 {
		t.Fatal("rejected endpoint opened a connection")
	}
}

func TestRunChecksRejectsNilPool(t *testing.T) {
	result, err := RunChecks(context.Background(), nil)
	if result != nil || err == nil || !strings.Contains(err.Error(), "connection pool") {
		t.Fatalf("expected pool boundary error, result=%v error=%v", result, err)
	}
}
