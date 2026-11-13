package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Integration (migration-11b): a real token from the real store calls the
// lint route on the full router, against a real catalog. Skipped without
// SAGE_TEST_DATABASE_URL.

// lintTokenTable makes lint_tok.big with 20000 analyzed rows.
func lintTokenTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE SCHEMA IF NOT EXISTS lint_tok`,
		`DROP TABLE IF EXISTS lint_tok.big`,
		`CREATE TABLE lint_tok.big (id int, c int)`,
		`INSERT INTO lint_tok.big SELECT g, g FROM generate_series(1, 20000) g`,
		`ANALYZE lint_tok.big`,
	} {
		_, err := pool.Exec(ctx, sql)
		require.NoError(t, err, sql)
	}
}

func lintToken(t *testing.T, pool *pgxpool.Pool, owner int,
	databases ...string) mcptoken.Token {
	t.Helper()
	tok, err := mcptoken.NewStore(pool).Create(context.Background(), mcptoken.CreateRequest{
		Name: "ci-lint-" + time.Now().Format("150405.000000000"), Kind: mcptoken.KindOperator,
		Scopes: []string{mcptoken.ScopeRead}, Databases: databases, ExpiresIn: time.Hour,
		OwnerUserID: owner, CreatedBy: "migration-11b test"})
	require.NoError(t, err)
	return tok
}

func TestMigrationLintTokenRealCatalog(t *testing.T) {
	pool := surfacePool(t)
	lintTokenTable(t, pool)
	cfg := config.DefaultConfig()
	cfg.Migration.DDLRowThreshold = 1000
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "alpha", Pool: pool,
		Config: config.DatabaseConfig{Name: "alpha"}})
	h := newTestRouter(mgr, cfg, pool, nil, nil, nil, &RuntimeDeps{},
		SessionAuthMiddleware(pool))
	owner := tokenRouteUser(t, pool, auth.RoleOperator)
	tok := lintToken(t, pool, owner.ID, "*")
	body := `{"sql":"ALTER TABLE lint_tok.big ALTER COLUMN c SET NOT NULL","database":"alpha"}`

	rec := postLintAuth(t, h, nil, "Bearer "+tok.Secret, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rep migration.Report
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rep))
	require.Equal(t, "risky", rep.Verdict, rec.Body.String())
	require.False(t, rep.Static)

	other := lintToken(t, pool, owner.ID, "other")
	rec = postLintAuth(t, h, nil, "Bearer "+other.Secret, body)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	rec = postLintAuth(t, h, nil, "", body)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	_, err := mcptoken.NewStore(pool).Revoke(context.Background(), tok.ID,
		"migration-11b test", mcptoken.Audit{})
	require.NoError(t, err)
	rec = postLintAuth(t, h, nil, "Bearer "+tok.Secret, body)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}
