package mcptoken_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// List carries each agent token's principal status (agentgov-01), so the
// MCP Tokens page can show a retired agent's token as refused although it
// is neither revoked nor expired. No concurrency test: List is one
// read-only statement and adds no shared state.

// listPrincipal inserts an active principal of its own, so a status change
// in one test cannot leak into another.
func listPrincipal(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := agentguard.NewID()
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `INSERT INTO sage.guard_principals
		(id, name, profile, created_by) VALUES ($1, $2, 'legacy', 'test')`,
		id, uniq("list-status"))
	require.NoError(t, err)
	return id
}

func listedByID(t *testing.T, s *mcptoken.Store, id string) mcptoken.Token {
	t.Helper()
	tokens, err := s.List(context.Background())
	require.NoError(t, err)
	for _, tok := range tokens {
		if tok.ID == id {
			return tok
		}
	}
	t.Fatalf("token %s not listed", id)
	return mcptoken.Token{}
}

func TestListPrincipalStatusFollowsTheAgent(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	principals := agentguard.NewStore(pool)
	principal := listPrincipal(t, pool)
	req := agentReq(uniq("list-status-agent"))
	req.PrincipalID = principal
	tok := mustCreate(t, s, req)

	got := listedByID(t, s, tok.ID)
	require.Equal(t, principal, got.PrincipalID)
	require.Equal(t, "active", got.PrincipalStatus)

	_, err := principals.SetStatus(ctx, principal, agentguard.StatusFrozen,
		agentguard.Audit{Reason: "incident"})
	require.NoError(t, err)
	require.Equal(t, "frozen", listedByID(t, s, tok.ID).PrincipalStatus)
	// A frozen agent's token still authenticates (it then gets agent_frozen).
	_, err = s.Validate(ctx, tok.Secret)
	require.NoError(t, err)

	_, err = principals.SetStatus(ctx, principal, agentguard.StatusRetired, agentguard.Audit{})
	require.NoError(t, err)
	retired := listedByID(t, s, tok.ID)
	require.Equal(t, "retired", retired.PrincipalStatus)
	require.Nil(t, retired.RevokedAt, "the row is neither revoked nor expired")
	// The status the page shows matches what Validate does: refused.
	_, err = s.Validate(ctx, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
}

func TestListOperatorTokenHasNoPrincipalStatus(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, s, operatorReq(uniq("list-status-op"), owner))

	got := listedByID(t, s, tok.ID)
	require.Equal(t, "", got.PrincipalID)
	require.Equal(t, "", got.PrincipalStatus)
	require.NotNil(t, got.OwnerUserID)
	require.Equal(t, owner, *got.OwnerUserID)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "principal_status")
}

func TestListPrincipalStatusSerializes(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	tok := mustCreate(t, s, agentReq(uniq("list-status-json")))
	raw, err := json.Marshal(listedByID(t, s, tok.ID))
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "active", out["principal_status"])
	_, hasSecret := out["token"]
	require.False(t, hasSecret, "a listed token never carries its secret")
}
