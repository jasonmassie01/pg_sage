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
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-06 on the real router and PostgreSQL: outside single-operator
// mode a widening PATCH answers 202 with a pending change, and a second
// admin (not the requester, not the sponsor) confirms the same patch.

type widenFixture struct {
	pool                *pgxpool.Pool
	a, b, sponsor       *auth.User
	asA, asB, asSponsor http.Handler
	id, path            string
}

func newWidenFixture(t *testing.T) widenFixture {
	t.Helper()
	pool := surfacePool(t)
	f := widenFixture{pool: pool, a: tokenRouteUser(t, pool, auth.RoleAdmin),
		b: tokenRouteUser(t, pool, auth.RoleAdmin), sponsor: tokenRouteUser(t, pool,
			auth.RoleAdmin)}
	f.asA, f.asB = agentRouter(t, pool, f.a, false), agentRouter(t, pool, f.b, false)
	f.asSponsor = agentRouter(t, pool, f.sponsor, false)
	f.id = createAgent(t, f.asA, f.sponsor.ID, "stage")["id"].(string)
	f.path = agentsPath + "/" + f.id
	return f
}

// widen sends a widening PATCH as a and returns the pending change id.
func (f widenFixture) widen(t *testing.T, body string) int64 {
	t.Helper()
	resp := callOK(t, f.asA, http.MethodPatch, f.path, body, http.StatusAccepted)
	got := decodeObject(t, resp)
	require.Equal(t, "pending", got["status"], resp)
	require.NotEmpty(t, got["expires_at"], resp)
	id, ok := got["change_id"].(float64)
	require.True(t, ok && id > 0, "change_id in %s", resp)
	return int64(id)
}

func (f widenFixture) confirmPath(cid int64) string {
	return fmt.Sprintf("%s/changes/%d/confirm", f.path, cid)
}

func (f widenFixture) ceiling(t *testing.T) agentguard.Env {
	t.Helper()
	p, err := agentguard.NewStore(f.pool).Get(context.Background(), f.id)
	require.NoError(t, err)
	return p.EnvCeiling
}

func TestAgentWiden_SecondAdminConfirms(t *testing.T) {
	f := newWidenFixture(t)
	cid := f.widen(t, `{"env_ceiling":"prod","reason":"reads the prod replica"}`)
	require.Equal(t, agentguard.EnvStage, f.ceiling(t), "a pending change applies nothing")

	resp := callOK(t, f.asB, http.MethodGet, f.path+"/changes", "", http.StatusOK)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp), &list))
	require.Len(t, list.Items, 1, resp)
	require.Equal(t, float64(cid), list.Items[0]["change_id"])
	require.Equal(t, "prod", list.Items[0]["patch"].(map[string]any)["env_ceiling"])

	resp = callOK(t, f.asA, http.MethodPost, f.confirmPath(cid), `{"env_ceiling":"prod"}`,
		http.StatusForbidden)
	require.Contains(t, resp, "second_admin_required")
	resp = callOK(t, f.asSponsor, http.MethodPost, f.confirmPath(cid),
		`{"env_ceiling":"prod"}`, http.StatusForbidden)
	require.Contains(t, resp, "sponsor_cannot_approve")
	resp = callOK(t, f.asB, http.MethodPost, f.confirmPath(cid), `{"env_ceiling":"dev"}`,
		http.StatusConflict)
	require.Contains(t, resp, "change_mismatch")
	require.Equal(t, agentguard.EnvStage, f.ceiling(t), "refusals apply nothing")

	ev := oneEvent(t, f.pool, f.b.ID, auth.AuditAgentChangeConfirmed, func() {
		resp = callOK(t, f.asB, http.MethodPost, f.confirmPath(cid),
			`{"env_ceiling":"prod","reason":"checked with the team"}`, http.StatusOK)
	})
	require.Equal(t, "prod", decodeObject(t, resp)["env_ceiling"])
	require.Equal(t, float64(cid), ev.Detail["change_id"])
	require.Equal(t, agentguard.EnvProd, f.ceiling(t))
	resp = callOK(t, f.asB, http.MethodPost, f.confirmPath(cid), `{"env_ceiling":"prod"}`,
		http.StatusConflict)
	require.Contains(t, resp, "change_not_pending")
}

func TestAgentWiden_RequestIsAudited(t *testing.T) {
	f := newWidenFixture(t)
	var cid int64
	ev := oneEvent(t, f.pool, f.a.ID, auth.AuditAgentChangeRequested, func() {
		cid = f.widen(t, `{"profile":"app-writer","reason":"needs inserts"}`)
	})
	require.Equal(t, float64(cid), ev.Detail["change_id"])
	require.Equal(t, f.id, ev.Detail["principal_id"])
	require.Equal(t, "needs inserts", ev.Detail["reason"])
	require.Equal(t, "app-writer", ev.Detail["after"].(map[string]any)["profile"])
	require.Equal(t, f.sponsor.ID, ev.Target)
}

func TestAgentWiden_ExpiredChangeIs409(t *testing.T) {
	f := newWidenFixture(t)
	cid := f.widen(t, `{"env_ceiling":"prod"}`)
	_, err := f.pool.Exec(context.Background(), `UPDATE sage.guard_principal_changes
		SET expires_at = now() - interval '1 second' WHERE id = $1`, cid)
	require.NoError(t, err)
	resp := callOK(t, f.asB, http.MethodPost, f.confirmPath(cid), `{"env_ceiling":"prod"}`,
		http.StatusConflict)
	require.Contains(t, resp, "change_expired")
	var status string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT status
		FROM sage.guard_principal_changes WHERE id = $1`, cid).Scan(&status))
	require.Equal(t, "expired", status)
	require.Equal(t, agentguard.EnvStage, f.ceiling(t))
}

func TestAgentWiden_SecondWideningSupersedes(t *testing.T) {
	f := newWidenFixture(t)
	first := f.widen(t, `{"env_ceiling":"prod"}`)
	second := f.widen(t, `{"profile":"app-writer"}`)
	require.NotEqual(t, first, second)
	resp := callOK(t, f.asB, http.MethodPost, f.confirmPath(first), `{"env_ceiling":"prod"}`,
		http.StatusConflict)
	require.Contains(t, resp, "change_not_pending")
	resp = callOK(t, f.asB, http.MethodPost, f.confirmPath(second), `{"profile":"app-writer"}`,
		http.StatusOK)
	got := decodeObject(t, resp)
	require.Equal(t, "app-writer", got["profile"])
	require.Equal(t, "stage", got["env_ceiling"])
}

func TestAgentWiden_NarrowingProfileAppliesAtOnce(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	code, body := tokenCall(t, h, http.MethodPost, agentsPath,
		fmt.Sprintf(`{"name":%q,"sponsor_user_id":%d,"profile":"coding-agent",`+
			`"env_ceiling":"stage"}`, agentName("narrow"), admin.ID))
	require.Equal(t, http.StatusCreated, code, body)
	id := decodeObject(t, body)["id"].(string)
	resp := callOK(t, h, http.MethodPatch, agentsPath+"/"+id,
		`{"profile":"readonly-analyst"}`, http.StatusOK)
	require.Equal(t, "readonly-analyst", decodeObject(t, resp)["profile"],
		"coding-agent to readonly-analyst at stage narrows")
}

func TestAgentWiden_UnknownProfileIs422(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	for _, single := range []bool{false, true} {
		h := agentRouter(t, pool, admin, single)
		code, body := tokenCall(t, h, http.MethodPost, agentsPath,
			fmt.Sprintf(`{"name":%q,"sponsor_user_id":%d,"profile":"codng-agent",`+
				`"env_ceiling":"dev"}`, agentName("typo"), admin.ID))
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
		require.Contains(t, body, "readonly-analyst", "the refusal names the profiles")
		id := createAgent(t, h, admin.ID, "dev")["id"].(string)
		code, body = tokenCall(t, h, http.MethodPatch, agentsPath+"/"+id,
			`{"profile":"codng-agent","reason":"typo"}`)
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
	}
}

func TestAgentWiden_ConfirmRefusals(t *testing.T) {
	f := newWidenFixture(t)
	cid := f.widen(t, `{"env_ceiling":"prod"}`)
	operator := agentRouter(t, f.pool, tokenRouteUser(t, f.pool, auth.RoleOperator), false)
	callOK(t, operator, http.MethodPost, f.confirmPath(cid), `{"env_ceiling":"prod"}`,
		http.StatusForbidden)
	callOK(t, f.asB, http.MethodPost, f.path+"/changes/abc/confirm", `{}`,
		http.StatusNotFound)
	callOK(t, f.asB, http.MethodPost, f.path+"/changes/0/confirm", `{}`, http.StatusNotFound)
	callOK(t, f.asB, http.MethodPost, f.confirmPath(cid+1_000_000), `{"env_ceiling":"prod"}`,
		http.StatusNotFound)
	callOK(t, f.asB, http.MethodPost, f.confirmPath(cid),
		`{"env_ceiling":"prod","superuser":true}`, http.StatusUnprocessableEntity)
	callOK(t, f.asB, http.MethodPost, f.confirmPath(cid), `{}`, http.StatusUnprocessableEntity)
	callOK(t, f.asA, http.MethodPatch, f.path, `{"env_ceiling":"prod","status":"retired"}`,
		http.StatusUnprocessableEntity)
	require.Equal(t, agentguard.EnvStage, f.ceiling(t))
}

func TestAgentWiden_RefusedConfirmAuditIs500(t *testing.T) {
	f := newWidenFixture(t)
	ctx := context.Background()
	cid := f.widen(t, `{"env_ceiling":"prod"}`)
	name := strings.ReplaceAll(agentName("refuse-confirm"), "-", "_")
	_, err := f.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION sage.%s() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.detail->>'principal_id' = '%s' AND NEW.event = 'agent_change_confirmed'
		THEN RAISE EXCEPTION 'audit refused'; END IF;
		RETURN NEW; END $$`, name, f.id))
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON sage.auth_audit
		FOR EACH ROW EXECUTE FUNCTION sage.%s()`, name, name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+name+
			" ON sage.auth_audit")
		_, _ = f.pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS sage."+name+"()")
	})
	resp := callOK(t, f.asB, http.MethodPost, f.confirmPath(cid), `{"env_ceiling":"prod"}`,
		http.StatusInternalServerError)
	require.NotContains(t, resp, "audit refused", "storage errors stay hidden")
	require.Equal(t, agentguard.EnvStage, f.ceiling(t), "no change without its event")
}

func TestAgentWiden_ListServesTheProfiles(t *testing.T) {
	pool := surfacePool(t)
	h := agentRouter(t, pool, tokenRouteUser(t, pool, auth.RoleOperator), false)
	resp := callOK(t, h, http.MethodGet, agentsPath+"?limit=1", "", http.StatusOK)
	var page struct {
		Items    []map[string]any `json:"items"`
		Profiles []struct {
			Name       string   `json:"name"`
			Classes    []string `json:"classes"`
			EnvCeiling string   `json:"env_ceiling"`
			DirectLane bool     `json:"direct_lane"`
		} `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp), &page))
	require.NotNil(t, page.Items, "items stay in the page")
	require.Len(t, page.Profiles, 4, resp)
	require.Equal(t, "app-writer", page.Profiles[0].Name)
	require.True(t, page.Profiles[0].DirectLane)
	require.Equal(t, "readonly-analyst", page.Profiles[3].Name)
	require.Equal(t, []string{"read"}, page.Profiles[3].Classes)
	require.Equal(t, "prod", page.Profiles[3].EnvCeiling)
}
