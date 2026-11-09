package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// observability-08: sage.mcp_tokens.scopes admits the metrics scope, alone
// and only on an operator token; the read/propose/approve rule is kept.

const legacyMCPTokenScopesCheck = `
ALTER TABLE sage.mcp_tokens DROP CONSTRAINT IF EXISTS mcp_tokens_scopes_v2_check;
ALTER TABLE sage.mcp_tokens DROP CONSTRAINT IF EXISTS mcp_tokens_scopes_check;
ALTER TABLE sage.mcp_tokens ADD CONSTRAINT mcp_tokens_scopes_check
    CHECK (scopes <@ ARRAY['read', 'propose', 'approve'] AND 'read' = ANY (scopes));
`

func scopeChecks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conrelid = 'sage.mcp_tokens'::regclass
		AND contype = 'c' AND conname LIKE 'mcp_tokens_scopes%'`)
	if err != nil {
		t.Fatalf("reading scope checks: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scanning scope check: %v", err)
		}
		out[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("scope check rows: %v", err)
	}
	return out
}

func requireMetricsScopeCheck(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	checks := scopeChecks(t, ctx, pool)
	def, ok := checks["mcp_tokens_scopes_v2_check"]
	if !ok || len(checks) != 1 {
		t.Fatalf("scope checks = %v, want only mcp_tokens_scopes_v2_check", checks)
	}
	if !strings.Contains(def, "metrics") || !strings.Contains(def, "approve") {
		t.Fatalf("scope check %q does not admit metrics beside read/propose/approve", def)
	}
}

func insertToken(ctx context.Context, pool *pgxpool.Pool, kind, scopes string,
	owner *int, hash string) error {
	_, err := pool.Exec(ctx, `INSERT INTO sage.mcp_tokens (name, kind, scopes,
		databases, token_hash, prefix, owner_user_id, created_by, expires_at)
		VALUES ('obs08', $1, $2::text[], '{*}', $3, 'pgs_mcp_x', $4, 'test',
		now() + interval '1 hour')`, kind, scopes, hash, owner)
	return err
}

func requireCheckViolationErr(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("%s: err = %v, want check_violation", what, err)
	}
}

func TestMCPTokensMetricsScopeMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	requireMetricsScopeCheck(t, ctx, pool)
	if _, err := pool.Exec(ctx, legacyMCPTokenScopesCheck); err != nil {
		t.Fatalf("simulating the pre-observability-08 schema: %v", err)
	}
	// Twice: the migration is idempotent on an upgraded schema too.
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireMetricsScopeCheck(t, ctx, pool)

	uid := sessionsMigrationUser(t, ctx, pool, "obs08-metrics@example.com")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.mcp_tokens WHERE name = 'obs08'`)
		_, _ = pool.Exec(bg, `DELETE FROM sage.users WHERE id = $1`, uid)
	})
	if err := insertToken(ctx, pool, "operator", "{metrics}", &uid,
		strings.Repeat("a", 43)); err != nil {
		t.Fatalf("a metrics-only operator token was refused: %v", err)
	}
	if err := insertToken(ctx, pool, "operator", "{read,propose}", &uid,
		strings.Repeat("b", 43)); err != nil {
		t.Fatalf("a read/propose operator token was refused: %v", err)
	}
	for _, tc := range []struct{ kind, scopes string }{
		{"operator", "{read,metrics}"}, {"operator", "{propose}"},
		{"operator", "{bogus}"}, {"agent", "{metrics}"},
	} {
		var owner *int
		if tc.kind == "operator" {
			owner = &uid
		}
		err := insertToken(ctx, pool, tc.kind, tc.scopes, owner,
			strings.Repeat("c", 40)+tc.kind[:3])
		requireCheckViolationErr(t, err, tc.kind+" "+tc.scopes)
	}
}
