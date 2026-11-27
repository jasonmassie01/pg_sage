package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-05 on the real router and PostgreSQL: each governance route
// writes exactly one chained auth_audit row naming the admin, a viewer
// cannot sponsor, a frozen principal keeps its sponsor, and a failed audit
// write fails the change.

type routeEvent struct {
	ID     int64
	Event  string
	Actor  int
	Target int
	IP     string
	Raw    string
	Detail map[string]any
}

// eventsBy are the auth_audit rows written by actor after row id since.
func eventsBy(t *testing.T, pool *pgxpool.Pool, actor int, since int64) []routeEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, event, actor_user_id,
		target_user_id, source_ip, detail::text FROM sage.auth_audit
		WHERE actor_user_id = $1 AND id > $2 ORDER BY id`, actor, since)
	require.NoError(t, err)
	defer rows.Close()
	out := []routeEvent{}
	for rows.Next() {
		var e routeEvent
		require.NoError(t, rows.Scan(&e.ID, &e.Event, &e.Actor, &e.Target, &e.IP, &e.Raw))
		require.NoError(t, json.Unmarshal([]byte(e.Raw), &e.Detail))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

// oneEvent runs call and requires exactly one new event named want by actor.
func oneEvent(t *testing.T, pool *pgxpool.Pool, actor int, want string,
	call func()) routeEvent {
	t.Helper()
	var since int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(id), 0) FROM sage.auth_audit`).Scan(&since))
	call()
	ev := eventsBy(t, pool, actor, since)
	require.Len(t, ev, 1, "%s: events %+v", want, ev)
	require.Equal(t, want, ev[0].Event)
	require.Equal(t, "192.0.2.1", ev[0].IP, "the client address is recorded")
	links, err := auditchain.LinksForRow(context.Background(), pool, "auth_audit", ev[0].ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	rep, err := auditchain.Verify(context.Background(), pool, auditchain.AuthAudit,
		auditchain.Window{FromSeq: links[0].Seq, ToSeq: links[0].Seq})
	require.NoError(t, err)
	require.True(t, rep.OK(), "%s verifies in the chain: %+v", want, rep.Problems)
	return ev[0]
}

func callOK(t *testing.T, h http.Handler, method, path, body string, want int) string {
	t.Helper()
	code, resp := tokenCall(t, h, method, path, body)
	require.Equal(t, want, code, "%s %s: %s", method, path, resp)
	return resp
}

func TestAgentsAudit_EachRouteWritesOneEvent(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	other := tokenRouteUser(t, pool, auth.RoleOperator)
	h, single := agentRouter(t, pool, admin, false), agentRouter(t, pool, admin, true)
	var id string
	ev := oneEvent(t, pool, admin.ID, auth.AuditAgentCreated, func() {
		id = createAgent(t, h, admin.ID, "stage")["id"].(string)
	})
	require.Equal(t, id, ev.Detail["principal_id"])
	require.Equal(t, admin.ID, ev.Target)
	path := agentsPath + "/" + id

	ev = oneEvent(t, pool, admin.ID, auth.AuditAgentUpdated, func() {
		callOK(t, h, http.MethodPatch, path,
			fmt.Sprintf(`{"sponsor_user_id":%d,"env_ceiling":"dev"}`, other.ID), 200)
	})
	require.Equal(t, other.ID, ev.Target)
	before := ev.Detail["before"].(map[string]any)
	after := ev.Detail["after"].(map[string]any)
	require.Equal(t, float64(admin.ID), before["sponsor_user_id"])
	require.Equal(t, float64(other.ID), after["sponsor_user_id"])
	require.Equal(t, "stage", before["env_ceiling"])
	require.Equal(t, "dev", after["env_ceiling"])

	ev = oneEvent(t, pool, admin.ID, auth.AuditAgentWidenedSingleOperator, func() {
		callOK(t, single, http.MethodPatch, path,
			`{"env_ceiling":"prod","reason":"lifeos reads prod"}`, 200)
	})
	require.Equal(t, "lifeos reads prod", ev.Detail["reason"])
	require.Equal(t, "prod", ev.Detail["after"].(map[string]any)["env_ceiling"])

	auditMintAndRetire(t, pool, h, admin, other, id)
	auditOperatorTokenRoutes(t, pool, admin, other)
}

func auditMintAndRetire(t *testing.T, pool *pgxpool.Pool, h http.Handler,
	admin, sponsor *auth.User, id string) {
	t.Helper()
	path := agentsPath + "/" + id
	var secret string
	ev := oneEvent(t, pool, admin.ID, auth.AuditAgentTokenMinted, func() {
		resp := callOK(t, h, http.MethodPost, path+"/tokens", `{"name":"ci",`+
			`"scopes":["read"],"databases":["orders"],"expires_in_days":7}`, 201)
		secret, _ = decodeObject(t, resp)["token"].(string)
	})
	require.Equal(t, id, ev.Detail["principal_id"])
	require.Equal(t, sponsor.ID, ev.Target, "an agent token's target is the sponsor")
	require.NotEmpty(t, ev.Detail["token_id"])
	require.NotContains(t, ev.Raw, secret)
	require.NotContains(t, ev.Raw, mcptoken.HashSecret(secret))

	ev = oneEvent(t, pool, admin.ID, auth.AuditAgentRetired, func() {
		callOK(t, h, http.MethodPatch, path, `{"status":"retired","reason":"done"}`, 200)
	})
	require.Equal(t, "done", ev.Detail["reason"])
	require.Equal(t, "active", ev.Detail["before_status"])
}

func auditOperatorTokenRoutes(t *testing.T, pool *pgxpool.Pool, admin, owner *auth.User) {
	t.Helper()
	h := tokenRouter(t, pool, admin)
	var tokID, secret string
	ev := oneEvent(t, pool, admin.ID, auth.AuditMCPTokenCreated, func() {
		resp := callOK(t, h, http.MethodPost, "/api/v1/mcp/tokens", fmt.Sprintf(
			`{"name":"desk","kind":"operator","scopes":["read","approve"],`+
				`"databases":["*"],"expires_in_days":3,"owner_user_id":%d}`, owner.ID), 201)
		tok := decodeObject(t, resp)
		tokID, _ = tok["id"].(string)
		secret, _ = tok["token"].(string)
	})
	require.Equal(t, owner.ID, ev.Target)
	require.Equal(t, tokID, ev.Detail["token_id"])
	require.NotContains(t, ev.Raw, secret)
	ev = oneEvent(t, pool, admin.ID, auth.AuditMCPTokenRevoked, func() {
		callOK(t, h, http.MethodDelete, "/api/v1/mcp/tokens/"+tokID, "", 200)
	})
	require.Equal(t, tokID, ev.Detail["token_id"])
	require.Equal(t, owner.ID, ev.Target)
}

func TestAgentsAudit_ViewerCannotSponsor(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	viewer := tokenRouteUser(t, pool, auth.RoleViewer)
	h := agentRouter(t, pool, admin, false)
	resp := callOK(t, h, http.MethodPost, agentsPath,
		agentBody(agentName("viewer-sponsored"), viewer.ID, "dev"), 422)
	require.Contains(t, resp, "sponsor")
	id := createAgent(t, h, admin.ID, "dev")["id"].(string)
	resp = callOK(t, h, http.MethodPatch, agentsPath+"/"+id,
		fmt.Sprintf(`{"sponsor_user_id":%d}`, viewer.ID), 422)
	require.Contains(t, resp, "operator or admin")
	p, err := agentguard.NewStore(pool).Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, admin.ID, *p.SponsorUserID, "a refused sponsor changes nothing")
}

func TestAgentsAudit_SponsorChangeFrozenIs409(t *testing.T) {
	pool := surfacePool(t)
	ctx := context.Background()
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	other := tokenRouteUser(t, pool, auth.RoleOperator)
	h := agentRouter(t, pool, admin, false)
	id := createAgent(t, h, admin.ID, "dev")["id"].(string)
	_, err := pool.Exec(ctx, `UPDATE sage.guard_principals SET status = 'frozen',
		frozen_reason = 'kill' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO sage.guard_freezes (scope, target, reason, set_by)
		VALUES ('principal', $1, 'kill', 'test')`, id)
	require.NoError(t, err)
	body := fmt.Sprintf(`{"sponsor_user_id":%d}`, other.ID)
	resp := callOK(t, h, http.MethodPatch, agentsPath+"/"+id, body, http.StatusConflict)
	require.Contains(t, resp, "sponsor_change_frozen")

	_, err = pool.Exec(ctx, `UPDATE sage.guard_freezes SET cleared_at = now(),
		cleared_by = 'test' WHERE scope = 'principal' AND target = $1`, id)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE sage.guard_principals SET status = 'active',
		frozen_reason = '' WHERE id = $1`, id)
	require.NoError(t, err)
	resp = callOK(t, h, http.MethodPatch, agentsPath+"/"+id, body, http.StatusOK)
	require.Equal(t, float64(other.ID), decodeObject(t, resp)["sponsor_user_id"])
}

func TestAgentsAudit_RefusedAuditIs500AndChangesNothing(t *testing.T) {
	pool := surfacePool(t)
	ctx := context.Background()
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	name := strings.ReplaceAll(agentName("refuse-audit"), "-", "_")
	id := createAgent(t, h, admin.ID, "stage")["id"].(string)
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION sage.%s() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.detail->>'principal_id' = '%s' THEN RAISE EXCEPTION 'audit refused'; END IF;
		RETURN NEW; END $$`, name, id))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON sage.auth_audit
		FOR EACH ROW EXECUTE FUNCTION sage.%s()`, name, name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+name+
			" ON sage.auth_audit")
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS sage."+name+"()")
	})
	resp := callOK(t, h, http.MethodPatch, agentsPath+"/"+id, `{"env_ceiling":"dev"}`,
		http.StatusInternalServerError)
	require.NotContains(t, resp, "audit refused", "storage errors stay hidden")
	p, err := agentguard.NewStore(pool).Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, agentguard.EnvStage, p.EnvCeiling, "no change without its event")
	callOK(t, h, http.MethodPatch, agentsPath+"/"+id, `{"status":"retired"}`,
		http.StatusInternalServerError)
	p, err = agentguard.NewStore(pool).Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, agentguard.StatusActive, p.Status, "no retirement without its event")
}
