package recommendation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// ci-15 happy path: this package opts into the bootstrapped fixture
// template, so a database it creates already holds the migration ledger
// and schema.Bootstrap on it only checks (under 2 s on an idle PG17).
func TestCreatedDatabaseComesBootstrapped(t *testing.T) {
	_ = testdb.SkipUnlessLive(t)
	if tpl := testdb.FixtureTemplate(); !strings.HasPrefix(tpl, "pgsage_tpl_") {
		t.Fatalf("FixtureTemplate() = %q, want the bootstrapped template", tpl)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateBootstrappedDatabase(t, "tplhappy"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var rows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.schema_migrations").Scan(&rows); err != nil {
		t.Fatalf("read the ledger before bootstrap: %v", err)
	}
	if rows < 100 {
		t.Fatalf("the copied database has %d ledger rows, want the full ledger", rows)
	}
	start := time.Now()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("bootstrap on the copied database took %s, want under 2s", took)
	}
}
