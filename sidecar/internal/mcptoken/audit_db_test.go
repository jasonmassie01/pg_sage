package mcptoken_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-05: minting and revoking a token writes one sage.auth_audit
// row in the same transaction; the row never carries the secret or its
// hash, and a failed audit write fails the change.

type tokenEvent struct {
	Event  string
	Actor  int
	Target int
	IP     string
	Detail map[string]any
	Raw    string
}

func tokenEvents(t *testing.T, pool *pgxpool.Pool, tokenID string) []tokenEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT event, actor_user_id,
		target_user_id, source_ip, detail::text FROM sage.auth_audit
		WHERE detail->>'token_id' = $1 ORDER BY id`, tokenID)
	require.NoError(t, err)
	defer rows.Close()
	out := []tokenEvent{}
	for rows.Next() {
		var e tokenEvent
		require.NoError(t, rows.Scan(&e.Event, &e.Actor, &e.Target, &e.IP, &e.Raw))
		require.NoError(t, json.Unmarshal([]byte(e.Raw), &e.Detail))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

func requireNoSecret(t *testing.T, e tokenEvent, tok mcptoken.Token) {
	t.Helper()
	require.NotContains(t, e.Raw, tok.Secret)
	require.NotContains(t, e.Raw, mcptoken.HashSecret(tok.Secret))
	require.NotContains(t, e.Raw, mcptoken.SecretPrefix)
}

func TestAudit_OperatorTokenCreateAndRevoke(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ctx := context.Background()
	owner, actor := createUser(t, pool, "operator"), createUser(t, pool, "admin")
	req := operatorReq(uniq("audited-op"), owner)
	req.Audit = mcptoken.Audit{ActorUserID: actor, SourceIP: "198.51.100.7"}
	tok := mustCreate(t, s, req)
	ev := tokenEvents(t, pool, tok.ID)
	require.Len(t, ev, 1)
	require.Equal(t, auth.AuditMCPTokenCreated, ev[0].Event)
	require.Equal(t, actor, ev[0].Actor)
	require.Equal(t, owner, ev[0].Target, "target_user_id is the owner")
	require.Equal(t, "198.51.100.7", ev[0].IP)
	require.Equal(t, req.Name, ev[0].Detail["name"])
	require.Equal(t, "operator", ev[0].Detail["kind"])
	require.Equal(t, []any{"approve", "propose", "read"}, sortedAny(ev[0].Detail["scopes"]))
	require.Equal(t, []any{"*"}, ev[0].Detail["databases"])
	exp, err := time.Parse(time.RFC3339Nano, fmt.Sprint(ev[0].Detail["expires_at"]))
	require.NoError(t, err)
	require.True(t, exp.Sub(tok.ExpiresAt).Abs() < time.Second, "expires_at %v", exp)
	requireNoSecret(t, ev[0], tok)

	_, err = s.Revoke(ctx, tok.ID, "admin@example.com", mcptoken.Audit{ActorUserID: actor})
	require.NoError(t, err)
	_, err = s.Revoke(ctx, tok.ID, "again@example.com", mcptoken.Audit{ActorUserID: actor})
	require.NoError(t, err)
	ev = tokenEvents(t, pool, tok.ID)
	require.Len(t, ev, 2, "a repeated revoke changes nothing and writes nothing")
	require.Equal(t, auth.AuditMCPTokenRevoked, ev[1].Event)
	require.Equal(t, owner, ev[1].Target)
	require.Equal(t, req.Name, ev[1].Detail["name"])
	requireNoSecret(t, ev[1], tok)
}

func TestAudit_AgentTokenMinted(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	tok := mustCreate(t, s, agentReq(uniq("audited-agent"), "orders", "billing"))
	ev := tokenEvents(t, pool, tok.ID)
	require.Len(t, ev, 1)
	require.Equal(t, auth.AuditAgentTokenMinted, ev[0].Event)
	require.Equal(t, testPrincipalID, ev[0].Detail["principal_id"])
	require.Equal(t, 0, ev[0].Target, "the test principal has no sponsor")
	require.Equal(t, []any{"billing", "orders"}, sortedAny(ev[0].Detail["databases"]))
	requireNoSecret(t, ev[0], tok)
}

func TestAudit_FailedWriteFailsTokenChange(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ctx := context.Background()
	live := mustCreate(t, s, agentReq(uniq("auditrefuse-live")))
	ctxName := strings.ReplaceAll(uniq("auditrefuse"), "-", "_")
	_, err := pool.Exec(ctx, `UPDATE sage.mcp_tokens SET name = $2 WHERE id = $1::uuid`,
		live.ID, ctxName+"_live")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION sage.%s() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.detail->>'name' LIKE '%s%%' THEN RAISE EXCEPTION 'audit refused'; END IF;
		RETURN NEW; END $$`, ctxName, ctxName))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON sage.auth_audit
		FOR EACH ROW EXECUTE FUNCTION sage.%s()`, ctxName, ctxName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+ctxName+
			" ON sage.auth_audit")
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS sage."+ctxName+"()")
	})

	_, err = s.Create(ctx, agentReq(ctxName+"_new"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "audit")
	require.Equal(t, 0, rowCount(t, pool, ctxName+"_new"), "no token without its event")
	_, err = s.Revoke(ctx, live.ID, "admin@example.com", mcptoken.Audit{})
	require.Error(t, err)
	_, err = s.Validate(ctx, live.Secret)
	require.NoError(t, err, "a refused audit leaves the token live")
}

func TestAudit_InvalidTokenAudit(t *testing.T) {
	s := mcptoken.NewStore(nil)
	req := agentReq("bad-audit")
	req.Audit = mcptoken.Audit{ActorUserID: -1}
	_, err := s.Create(context.Background(), req)
	require.ErrorIs(t, err, mcptoken.ErrInvalid)
	req.Audit = mcptoken.Audit{SourceIP: "1.2.3.4\x00"}
	_, err = s.Create(context.Background(), req)
	require.ErrorIs(t, err, mcptoken.ErrInvalid)
	_, err = s.Revoke(context.Background(), "00000000-0000-0000-0000-000000000000",
		"a@example.com", mcptoken.Audit{ActorUserID: -1})
	require.ErrorIs(t, err, mcptoken.ErrInvalid)
}

func sortedAny(v any) []any {
	in, _ := v.([]any)
	out := append([]any{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && fmt.Sprint(out[j]) < fmt.Sprint(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
