package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These checks must reject the route before DNS or schema writes. Real direct
// and session endpoint bootstrap is covered by the provider live suite.
func TestBootstrapRejectsHostedTransactionEndpoint(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://unused@ep-test-pooler.aws.neon.tech/test?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	err = Bootstrap(context.Background(), pool)
	if err == nil || !strings.Contains(err.Error(), "Neon direct endpoint") {
		t.Fatalf("expected actionable endpoint error before connecting, got %v", err)
	}
	if pool.Stat().TotalConns() != 0 {
		t.Fatal("rejected endpoint opened a connection")
	}
}

func TestBootstrapRejectsNilPool(t *testing.T) {
	err := Bootstrap(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "connection pool") {
		t.Fatalf("expected pool boundary error, got %v", err)
	}
}
