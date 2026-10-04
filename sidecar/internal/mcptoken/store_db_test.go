package mcptoken_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Integration tests against the live control database (sage.mcp_tokens).

var secretShape = regexp.MustCompile(`^pgs_mcp_[A-Za-z0-9_-]{43}$`)

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, schema.Bootstrap(ctx, pool))
	return pool
}

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func createUser(t *testing.T, pool *pgxpool.Pool, role string) int {
	t.Helper()
	email := uniq("mcptoken-"+role) + "@example.com"
	id, err := auth.CreateUser(context.Background(), pool, email, "password-123", role)
	require.NoError(t, err)
	require.Positive(t, id)
	return id
}

func agentReq(name string, databases ...string) mcptoken.CreateRequest {
	if len(databases) == 0 {
		databases = []string{"orders"}
	}
	return mcptoken.CreateRequest{
		Name: name, Kind: mcptoken.KindAgent, Scopes: []string{"read", "propose"},
		Databases: databases, ExpiresIn: day, CreatedBy: "admin@example.com",
	}
}

func operatorReq(name string, owner int) mcptoken.CreateRequest {
	req := agentReq(name, "*")
	req.Kind = mcptoken.KindOperator
	req.Scopes = []string{"read", "propose", "approve"}
	req.OwnerUserID = owner
	return req
}

func mustCreate(t *testing.T, s *mcptoken.Store, req mcptoken.CreateRequest) mcptoken.Token {
	t.Helper()
	tok, err := s.Create(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, tok.ID)
	require.True(t, secretShape.MatchString(tok.Secret), "secret shape: %q", tok.Secret)
	return tok
}

func listed(t *testing.T, s *mcptoken.Store, id string) (mcptoken.Token, bool) {
	t.Helper()
	tokens, err := s.List(context.Background())
	require.NoError(t, err)
	for _, tok := range tokens {
		if tok.ID == id {
			return tok, true
		}
	}
	return mcptoken.Token{}, false
}

func rowCount(t *testing.T, pool *pgxpool.Pool, name string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.mcp_tokens WHERE name = $1`, name).Scan(&n)
	require.NoError(t, err)
	return n
}

func TestCreateAgentTokenReturnsSecretOnce(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	name := uniq("agent")
	tok := mustCreate(t, s, agentReq(name))

	require.Equal(t, name, tok.Name)
	require.Equal(t, mcptoken.KindAgent, tok.Kind)
	require.Equal(t, []string{"read", "propose"}, tok.Scopes)
	require.Equal(t, []string{"orders"}, tok.Databases)
	require.Nil(t, tok.OwnerUserID)
	require.Equal(t, "admin@example.com", tok.CreatedBy)
	require.Equal(t, tok.Secret[:12], tok.Prefix)
	require.WithinDuration(t, tok.CreatedAt.Add(day), tok.ExpiresAt, time.Minute)
	require.Nil(t, tok.RevokedAt)
	require.Nil(t, tok.LastUsedAt)

	all, err := s.List(context.Background())
	require.NoError(t, err)
	got, ok := listed(t, s, tok.ID)
	require.True(t, ok, "created token must be listed")
	require.Equal(t, "", got.Secret)
	require.Equal(t, tok.Prefix, got.Prefix)
	raw, err := json.Marshal(all)
	require.NoError(t, err)
	require.NotContains(t, string(raw), tok.Secret)
	require.NotContains(t, string(raw), mcptoken.HashSecret(tok.Secret))
	for _, other := range all {
		require.Equal(t, "", other.Secret, "List must never return a secret")
	}
}

func TestStoredRowHoldsOnlyHashAndPrefix(t *testing.T) {
	pool := livePool(t)
	tok := mustCreate(t, mcptoken.NewStore(pool), agentReq(uniq("row")))
	ctx := context.Background()

	var row, hash, prefix string
	err := pool.QueryRow(ctx, `SELECT row_to_json(t)::text, t.token_hash, t.prefix
		FROM sage.mcp_tokens t WHERE t.id::text = $1`, tok.ID).Scan(&row, &hash, &prefix)
	require.NoError(t, err)
	require.NotContains(t, row, tok.Secret)
	require.NotContains(t, row, tok.Secret[len(mcptoken.SecretPrefix):])
	require.Equal(t, mcptoken.HashSecret(tok.Secret), hash)
	require.Equal(t, tok.Prefix, prefix)
}

func TestCreateNormalizesScopesAndDatabases(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	req := agentReq(uniq("norm"), "billing", "accounts", "billing")
	req.Scopes = []string{"propose", "read", "read"}
	tok := mustCreate(t, s, req)
	require.Equal(t, []string{"read", "propose"}, tok.Scopes)
	require.Equal(t, []string{"accounts", "billing"}, tok.Databases)

	owner := createUser(t, pool, auth.RoleAdmin)
	op := operatorReq(uniq("norm-op"), owner)
	op.Scopes = []string{"approve", "read", "propose"}
	opTok := mustCreate(t, s, op)
	require.Equal(t, []string{"read", "propose", "approve"}, opTok.Scopes)
	require.Equal(t, []string{"*"}, opTok.Databases)
	require.NotNil(t, opTok.OwnerUserID)
	require.Equal(t, owner, *opTok.OwnerUserID)
}

func TestValidateAgentGrantAndLastUsed(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	name := uniq("grant")
	tok := mustCreate(t, s, agentReq(name, "orders", "billing"))

	grant, err := s.Validate(context.Background(), tok.Secret)
	require.NoError(t, err)
	require.Equal(t, tok.ID, grant.TokenID)
	require.Equal(t, name, grant.Name)
	require.Equal(t, mcptoken.KindAgent, grant.Kind)
	require.Equal(t, []string{"read", "propose"}, grant.Scopes)
	require.Equal(t, []string{"billing", "orders"}, grant.Databases)
	require.Zero(t, grant.OwnerUserID)
	require.Equal(t, "", grant.OwnerRole)

	got, ok := listed(t, s, tok.ID)
	require.True(t, ok)
	require.NotNil(t, got.LastUsedAt, "Validate must record last_used_at")
	require.False(t, got.LastUsedAt.Before(got.CreatedAt))
}

func TestValidateStarDatabasesIsNil(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	tok := mustCreate(t, s, agentReq(uniq("star"), "*"))
	require.Equal(t, []string{"*"}, tok.Databases)
	grant, err := s.Validate(context.Background(), tok.Secret)
	require.NoError(t, err)
	require.Nil(t, grant.Databases, "[*] means every database: nil in the grant")
	require.Equal(t, tok.ID, grant.TokenID)
}

func TestRevokeLifecycle(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ctx := context.Background()
	tok := mustCreate(t, s, agentReq(uniq("revoke")))
	_, err := s.Validate(ctx, tok.Secret)
	require.NoError(t, err)

	revoked, err := s.Revoke(ctx, tok.ID, "admin@example.com")
	require.NoError(t, err)
	require.Equal(t, tok.ID, revoked.ID)
	require.NotNil(t, revoked.RevokedAt)
	require.Equal(t, "admin@example.com", revoked.RevokedBy)
	require.Equal(t, "", revoked.Secret)

	_, err = s.Validate(ctx, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)

	again, err := s.Revoke(ctx, tok.ID, "someone-else@example.com")
	require.NoError(t, err, "revoking a revoked token is idempotent")
	require.NotNil(t, again.RevokedAt)
	require.True(t, revoked.RevokedAt.Equal(*again.RevokedAt), "first revocation stands")
	require.Equal(t, "admin@example.com", again.RevokedBy)

	got, ok := listed(t, s, tok.ID)
	require.True(t, ok, "revoked tokens stay listed")
	require.NotNil(t, got.RevokedAt)
	require.Equal(t, "admin@example.com", got.RevokedBy)
}

func TestRevokeUnknownIsNotFound(t *testing.T) {
	s := mcptoken.NewStore(livePool(t))
	for _, id := range []string{"does-not-exist", "", "00000000-0000-0000-0000-000000000000",
		"999999999", "'; DROP TABLE sage.mcp_tokens; --"} {
		tok, err := s.Revoke(context.Background(), id, "admin@example.com")
		require.ErrorIs(t, err, mcptoken.ErrNotFound, "id %q", id)
		require.False(t, errors.Is(err, mcptoken.ErrUnauthorized))
		require.Equal(t, "", tok.ID)
	}
}

func TestValidateExpiredTokenIsUnauthorized(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ctx := context.Background()
	tok := mustCreate(t, s, agentReq(uniq("expiry")))

	_, err := pool.Exec(ctx, `UPDATE sage.mcp_tokens
		SET created_at = now() - interval '2 hours', expires_at = now() + interval '1 minute'
		WHERE id::text = $1`, tok.ID)
	require.NoError(t, err)
	grant, err := s.Validate(ctx, tok.Secret)
	require.NoError(t, err, "a token one minute from expiry is still valid")
	require.Equal(t, tok.ID, grant.TokenID)

	_, err = pool.Exec(ctx, `UPDATE sage.mcp_tokens SET expires_at = now() - interval '1 second'
		WHERE id::text = $1`, tok.ID)
	require.NoError(t, err)
	grant, err = s.Validate(ctx, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	require.Equal(t, "", grant.TokenID)
}

func TestValidateRejectsBadSecrets(t *testing.T) {
	s := mcptoken.NewStore(livePool(t))
	tok := mustCreate(t, s, agentReq(uniq("bad")))
	body := tok.Secret[len(mcptoken.SecretPrefix):]
	last := tok.Secret[len(tok.Secret)-1]
	flipped := byte('A')
	if last == 'A' {
		flipped = 'B'
	}
	bad := map[string]string{
		"empty":            "",
		"garbage":          "pgs_mcp_garbage",
		"prefix only":      mcptoken.SecretPrefix,
		"wrong prefix":     "pgs_xyz_" + body,
		"no prefix":        body,
		"last char flip":   tok.Secret[:len(tok.Secret)-1] + string(flipped),
		"extra char":       tok.Secret + "x",
		"padded":           " " + tok.Secret + " ",
		"upper prefix":     strings.ToUpper(mcptoken.SecretPrefix) + body,
		"hash as secret":   mcptoken.HashSecret(tok.Secret),
		"prefix with hash": mcptoken.SecretPrefix + mcptoken.HashSecret(tok.Secret),
	}
	for name, secret := range bad {
		grant, err := s.Validate(context.Background(), secret)
		require.ErrorIs(t, err, mcptoken.ErrUnauthorized, name)
		require.Equal(t, "", grant.TokenID, name)
	}
	grant, err := s.Validate(context.Background(), tok.Secret)
	require.NoError(t, err, "the real secret still validates")
	require.Equal(t, tok.ID, grant.TokenID)
}

func TestCreateAgentApproveInsertsNoRow(t *testing.T) {
	pool := livePool(t)
	name := uniq("agent-approve")
	req := agentReq(name)
	req.Scopes = []string{"read", "propose", "approve"}
	tok, err := mcptoken.NewStore(pool).Create(context.Background(), req)
	require.ErrorIs(t, err, mcptoken.ErrApproveForAgent)
	require.Equal(t, "", tok.Secret)
	require.Equal(t, 0, rowCount(t, pool, name))
}

func requireCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a Postgres error, got %v", err)
	require.Equal(t, "23514", pgErr.Code, "want check_violation, got %s", pgErr.Message)
}

// The approve/kind rule is enforced by the table too (defense in depth).
func TestDatabaseCheckRejectsAgentApprove(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	s := mcptoken.NewStore(pool)
	tok := mustCreate(t, s, agentReq(uniq("check")))

	var row string
	require.NoError(t, pool.QueryRow(ctx, `SELECT row_to_json(t)::text
		FROM sage.mcp_tokens t WHERE t.id::text = $1`, tok.ID).Scan(&row))
	_, err := pool.Exec(ctx, `DELETE FROM sage.mcp_tokens WHERE id::text = $1`, tok.ID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO sage.mcp_tokens SELECT * FROM
		jsonb_populate_record(NULL::sage.mcp_tokens,
		jsonb_set($1::jsonb, '{scopes}', '["read","propose","approve"]'))`, row)
	requireCheckViolation(t, err)

	// Control: the unmodified copy is accepted, so the CHECK is what failed.
	_, err = pool.Exec(ctx, `INSERT INTO sage.mcp_tokens SELECT * FROM
		jsonb_populate_record(NULL::sage.mcp_tokens, $1::jsonb)`, row)
	require.NoError(t, err)
	grant, err := s.Validate(ctx, tok.Secret)
	require.NoError(t, err)
	require.Equal(t, []string{"read", "propose"}, grant.Scopes)
}

func TestDatabaseCheckRejectsFlippingOperatorToAgent(t *testing.T) {
	pool := livePool(t)
	owner := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, mcptoken.NewStore(pool), operatorReq(uniq("flip"), owner))
	_, err := pool.Exec(context.Background(),
		`UPDATE sage.mcp_tokens SET kind = 'agent' WHERE id::text = $1`, tok.ID)
	require.Error(t, err, "an approve-holding row must not become an agent token")
	requireCheckViolation(t, err)
}

func setRole(t *testing.T, pool *pgxpool.Pool, id int, role string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE sage.users SET role = $1 WHERE id = $2`, role, id)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected())
}

func requireGrant(t *testing.T, s *mcptoken.Store, secret, role string, scopes []string) {
	t.Helper()
	grant, err := s.Validate(context.Background(), secret)
	require.NoError(t, err)
	require.Equal(t, mcptoken.KindOperator, grant.Kind)
	require.Equal(t, role, grant.OwnerRole)
	require.Equal(t, scopes, grant.Scopes)
	require.Nil(t, grant.Databases)
}

func TestOperatorTokenFollowsOwnerRole(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, s, operatorReq(uniq("op"), owner))
	all := []string{"read", "propose", "approve"}

	requireGrant(t, s, tok.Secret, auth.RoleOperator, all)
	grant, err := s.Validate(context.Background(), tok.Secret)
	require.NoError(t, err)
	require.Equal(t, owner, grant.OwnerUserID)

	setRole(t, pool, owner, auth.RoleViewer)
	requireGrant(t, s, tok.Secret, auth.RoleViewer, []string{"read"})

	setRole(t, pool, owner, auth.RoleAdmin)
	requireGrant(t, s, tok.Secret, auth.RoleAdmin, all)

	_, err = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, owner)
	require.NoError(t, err)
	_, err = s.Validate(context.Background(), tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	_, ok := listed(t, s, tok.ID)
	require.False(t, ok, "deleting the owner deletes the token")
}

func TestOperatorTokenScopesAreIntersection(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleAdmin)
	req := operatorReq(uniq("op-narrow"), owner)
	req.Scopes = []string{"read"}
	tok := mustCreate(t, s, req)
	requireGrant(t, s, tok.Secret, auth.RoleAdmin, []string{"read"})
}

func TestOperatorTokenOwnerMustBeOperatorOrAdmin(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	viewer := createUser(t, pool, auth.RoleViewer)
	for _, owner := range []int{viewer, 2147483000} {
		name := uniq("op-owner")
		tok, err := s.Create(context.Background(), operatorReq(name, owner))
		require.ErrorIs(t, err, mcptoken.ErrOwnerRequired, "owner %d", owner)
		require.False(t, errors.Is(err, mcptoken.ErrInvalid))
		require.Equal(t, "", tok.Secret)
		require.Equal(t, 0, rowCount(t, pool, name))
	}
	operator := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, s, operatorReq(uniq("op-ok"), operator))
	require.Equal(t, mcptoken.KindOperator, tok.Kind)
}

func TestListNewestFirst(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = mustCreate(t, s, agentReq(uniq(fmt.Sprintf("order-%d", i)))).ID
	}
	_, err := s.Revoke(context.Background(), ids[1], "admin@example.com")
	require.NoError(t, err)

	tokens, err := s.List(context.Background())
	require.NoError(t, err)
	pos := map[string]int{}
	for i, tok := range tokens {
		pos[tok.ID] = i
		if i > 0 {
			require.False(t, tok.CreatedAt.After(tokens[i-1].CreatedAt), "newest first")
		}
	}
	require.Less(t, pos[ids[2]], pos[ids[1]])
	require.Less(t, pos[ids[1]], pos[ids[0]])
	revoked := tokens[pos[ids[1]]]
	require.NotNil(t, revoked.RevokedAt)
	require.Equal(t, "admin@example.com", revoked.RevokedBy)
	require.Nil(t, tokens[pos[ids[0]]].RevokedAt)
}

func TestConcurrentValidate(t *testing.T) {
	s := mcptoken.NewStore(livePool(t))
	tok := mustCreate(t, s, agentReq(uniq("parallel")))
	const n = 20
	grants := make([]mcptoken.Grant, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			grants[i], errs[i] = s.Validate(context.Background(), tok.Secret)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "goroutine %d", i)
		require.Equal(t, tok.ID, grants[i].TokenID)
		require.Equal(t, []string{"read", "propose"}, grants[i].Scopes)
	}
}

func TestConcurrentCreateYieldsDistinctTokens(t *testing.T) {
	s := mcptoken.NewStore(livePool(t))
	const n = 10
	tokens := make([]mcptoken.Token, n)
	errs := make([]error, n)
	base := uniq("burst")
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = s.Create(context.Background(),
				agentReq(fmt.Sprintf("%s-%d", base, i)))
		}(i)
	}
	wg.Wait()
	ids, secrets := map[string]bool{}, map[string]bool{}
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "create %d", i)
		require.True(t, secretShape.MatchString(tokens[i].Secret))
		ids[tokens[i].ID] = true
		secrets[tokens[i].Secret] = true
		_, ok := listed(t, s, tokens[i].ID)
		require.True(t, ok)
	}
	require.Len(t, ids, n)
	require.Len(t, secrets, n)
}
