//go:build providerlive

package providerlive

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

func checkHypoPG(t *testing.T, f fixture) {
	h := optimizer.NewHypoPG(f.pool, 1, func(string, string, ...any) {})
	if !h.IsAvailable(t.Context()) {
		t.Skip("HypoPG extension unavailable; hypothetical-index capability NOT verified")
	}
	f.exec(t, "CREATE TABLE "+f.table("hypo_items")+" (id int, category int)")
	f.exec(t, "INSERT INTO "+f.table("hypo_items")+
		" SELECT i, i%10000 FROM generate_series(1,20000) i")
	f.exec(t, "ANALYZE "+f.table("hypo_items"))
	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "17s"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	checkError(t, "create isolated HypoPG session", err)
	defer pool.Close()
	// Neon may override connection-startup GUCs. Establish and verify the session baseline.
	_, err = pool.Exec(t.Context(), "SET statement_timeout='17s'")
	checkError(t, "set explicit HypoPG timeout baseline", err)
	var baseline string
	checkError(t, "verify HypoPG timeout baseline",
		pool.QueryRow(t.Context(), "SHOW statement_timeout").Scan(&baseline))
	if baseline != "17s" {
		t.Fatal("provider did not retain explicit session timeout")
	}
	h = optimizer.NewHypoPG(pool, 1, func(string, string, ...any) {})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	rec := optimizer.Recommendation{DDL: "CREATE INDEX candidate ON " + f.table("hypo_items") +
		" (category)"}
	queries := []optimizer.QueryInfo{{QueryID: 1,
		Text: "SELECT id FROM " + f.table("hypo_items") + " WHERE category=$1"}}
	ok, improvement, size, err := h.Validate(ctx, rec, queries)
	checkError(t, "validate hypothetical index on one session", err)
	if !ok || improvement <= 1 || size <= 0 {
		t.Fatalf("missing hypothetical evidence: accepted=%t improvement=%f size=%d",
			ok, improvement, size)
	}
	checkHypoPGCleanup(t, pool)
}

// This focused scenario owns only a random schema, so a monitor can keep its sage metadata.
func TestProviderNormalizedHypoPG(t *testing.T) {
	for _, provider := range []target{{"neon", "SAGE_NEON"}, {"supabase", "SAGE_SUPABASE"}} {
		t.Run(provider.name, func(t *testing.T) {
			dsn := os.Getenv(provider.prefix + "_DATABASE_URL")
			if dsn == "" {
				t.Skip(provider.prefix + "_DATABASE_URL absent; live check NOT verified")
			}
			pool := connect(t, dsn)
			f := newNamespaceFixture(t, pool, provider)
			checkHypoPG(t, f)
		})
	}
}

func checkHypoPGCleanup(t *testing.T, pool *pgxpool.Pool) {
	var timeout, extSchema string
	checkError(t, "read HypoPG timeout", pool.QueryRow(t.Context(),
		"SHOW statement_timeout").Scan(&timeout))
	if timeout != "17s" {
		t.Fatalf("HypoPG leaked timeout: %s", timeout)
	}
	checkError(t, "resolve HypoPG namespace", pool.QueryRow(t.Context(),
		`SELECT n.nspname FROM pg_catalog.pg_extension e
		JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='hypopg'`,
	).Scan(&extSchema))
	var indexes int
	checkError(t, "verify hypothetical cleanup", pool.QueryRow(t.Context(),
		"SELECT count(*) FROM "+pgx.Identifier{extSchema, "hypopg_list_indexes"}.Sanitize(),
	).Scan(&indexes))
	if indexes != 0 {
		t.Fatalf("HypoPG leaked %d indexes", indexes)
	}
}
