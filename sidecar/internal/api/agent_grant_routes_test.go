package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/agentguard/grants"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Agent grant routes (spec §8.3 shape, G1: every grant is L2): an
// operator lists, grants and revokes an agent's grants and approves or
// denies its capability requests. A denial is 409 with the reason and the
// exact fix; a malformed body 422; an unknown database or grant 404; no
// control database 503. Viewers are refused.

const testAgent = "agp_aaaaaaaaaaaaaaaaaaaa"

type fakeGrants struct {
	err      error
	userID   int
	database string
	pid      string
	id       int64
	in       grants.CapabilityRequest
	filter   grants.Filter
	rfilter  grants.RequestFilter
}

func (f *fakeGrants) Grants(_ context.Context, db string, flt grants.Filter) (grants.Page,
	error) {
	f.database, f.filter = db, flt
	return grants.Page{Items: []grants.Grant{{ID: 7, PrincipalID: flt.PrincipalID}}}, f.err
}

func (f *fakeGrants) GrantNow(_ context.Context, pid string, in grants.CapabilityRequest,
	user int) (grants.GrantResult, error) {
	f.pid, f.in, f.userID = pid, in, user
	return grants.GrantResult{ActionID: 11, Grants: []grants.Grant{{ID: 7}}}, f.err
}

func (f *fakeGrants) RevokeNow(_ context.Context, db, pid string, id int64,
	user int) (grants.RevokeResult, error) {
	f.database, f.pid, f.id, f.userID = db, pid, id, user
	return grants.RevokeResult{ActionID: 12, Grant: grants.Grant{ID: id,
		State: grants.StateRevoked}}, f.err
}

func (f *fakeGrants) Requests(_ context.Context, db string,
	flt grants.RequestFilter) (grants.RequestPage, error) {
	f.database, f.rfilter = db, flt
	return grants.RequestPage{Items: []grants.Request{{ID: 3}}}, f.err
}

func (f *fakeGrants) Approve(_ context.Context, db, pid string, id int64,
	user int) (grants.GrantResult, error) {
	f.database, f.pid, f.id, f.userID = db, pid, id, user
	return grants.GrantResult{ActionID: 13}, f.err
}

func (f *fakeGrants) Deny(_ context.Context, db, pid string, id int64,
	user int) (grants.Request, error) {
	f.database, f.pid, f.id, f.userID = db, pid, id, user
	return grants.Request{ID: id, Status: grants.RequestDenied}, f.err
}

func grantMux(svc AgentGrantService) *http.ServeMux {
	mux := http.NewServeMux()
	registerAgentGrantRoutes(mux, svc)
	return mux
}

func TestAgentGrantRoutes_RolesAndHappyPaths(t *testing.T) {
	f := &fakeGrants{}
	mux := grantMux(f)
	base := "/api/v1/agents/" + testAgent
	if code, _ := doJSON(t, mux, nil, "GET", base+"/grants?database=db1&limit=5",
		nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "GET", base+"/grants?database=db1",
		nil); code != http.StatusForbidden {
		t.Fatalf("viewer: %d", code)
	}
	code, body := doJSON(t, mux, operatorUser(), "GET",
		base+"/grants?database=db1&state=active&limit=5&cursor=9", nil)
	if code != http.StatusOK || f.database != "db1" || f.filter != (grants.Filter{
		PrincipalID: testAgent, State: "active", Limit: 5, Cursor: "9"}) {
		t.Fatalf("list: %d %v %+v", code, body, f.filter)
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("items: %v", body)
	}
	in := map[string]any{"database": "db1", "capability": "read", "duration_minutes": 30,
		"reason": "debug", "objects": []map[string]any{{"object": "app.orders",
			"columns": []string{"id"}}}}
	code, body = doJSON(t, mux, operatorUser(), "POST", base+"/grants", in)
	if code != http.StatusCreated || f.userID != operatorUser().ID || f.pid != testAgent ||
		f.in.DurationMinutes != 30 || f.in.Objects[0].Columns[0] != "id" ||
		body["action_id"] != float64(11) {
		t.Fatalf("grant: %d %v %+v", code, body, f.in)
	}
	code, body = doJSON(t, mux, operatorUser(), "POST", base+"/grants/7/revoke",
		map[string]any{"database": "db1"})
	if code != http.StatusOK || f.id != 7 || f.database != "db1" {
		t.Fatalf("revoke: %d %v", code, body)
	}
}

func TestAgentGrantRoutes_Requests(t *testing.T) {
	f := &fakeGrants{}
	mux := grantMux(f)
	base := "/api/v1/agents/" + testAgent + "/grant-requests"
	code, _ := doJSON(t, mux, operatorUser(), "GET", base+"?database=db1&status=pending"+
		"&limit=10", nil)
	if code != http.StatusOK || f.rfilter.Status != "pending" || f.rfilter.Limit != 10 {
		t.Fatalf("list: %d %+v", code, f.rfilter)
	}
	code, body := doJSON(t, mux, operatorUser(), "POST", base+"/3/approve",
		map[string]any{"database": "db1"})
	if code != http.StatusOK || f.id != 3 || f.pid != testAgent ||
		body["action_id"] != float64(13) {
		t.Fatalf("approve: %d %v", code, body)
	}
	code, body = doJSON(t, mux, operatorUser(), "POST", base+"/3/deny",
		map[string]any{"database": "db1"})
	if code != http.StatusOK || body["status"] != "denied" {
		t.Fatalf("deny: %d %v", code, body)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "POST", base+"/3/approve",
		map[string]any{"database": "db1"}); code != http.StatusForbidden {
		t.Fatalf("viewer approve: %d", code)
	}
}

func TestAgentGrantRoutes_Errors(t *testing.T) {
	base := "/api/v1/agents/" + testAgent
	in := map[string]any{"database": "db1", "capability": "read", "duration_minutes": 30,
		"reason": "x", "objects": []map[string]any{{"object": "app.orders"}}}
	cases := []struct {
		err    error
		status int
		reason string
	}{
		{&agentguard.DeniedError{Reason: agentguard.ReasonGrantorLacksPrivilege,
			Detail: "lacks", Fix: "GRANT x"}, http.StatusConflict, "grantor_lacks_privilege"},
		{fmt.Errorf("w: %w", agentguard.ErrInvalid), http.StatusUnprocessableEntity, ""},
		{fmt.Errorf("w: %w", agentguard.ErrNotFound), http.StatusNotFound, ""},
		{envbind.ErrUnknownDatabase, http.StatusNotFound, ""},
		{fmt.Errorf("w: %w", agentguard.ErrUnavailable), http.StatusServiceUnavailable, ""},
		{grants.ErrRequestNotPending, http.StatusConflict, ""},
		{grants.ErrNotActive, http.StatusConflict, ""},
		{&executor.WithheldError{Decision: executor.ActionPolicyDecision{
			BlockedReason: "observe_only"}}, http.StatusConflict, "observe_only"},
		{fmt.Errorf("boom"), http.StatusInternalServerError, ""},
	}
	for _, c := range cases {
		mux := grantMux(&fakeGrants{err: c.err})
		code, body := doJSON(t, mux, operatorUser(), "POST", base+"/grants", in)
		if code != c.status {
			t.Errorf("%v: status %d want %d (%v)", c.err, code, c.status, body)
		}
		if c.reason != "" && (body["reason_code"] != c.reason || body["verdict"] != "blocked") {
			t.Errorf("%v: body %v", c.err, body)
		}
	}
	denied := grantMux(&fakeGrants{err: &agentguard.DeniedError{
		Reason: agentguard.ReasonPublicCreate, Fix: "REVOKE CREATE ON SCHEMA s FROM PUBLIC;"}})
	_, body := doJSON(t, denied, operatorUser(), "POST", base+"/grants", in)
	if body["fix"] != "REVOKE CREATE ON SCHEMA s FROM PUBLIC;" {
		t.Fatalf("fix: %v", body)
	}
}

func TestAgentGrantRoutes_InvalidInput(t *testing.T) {
	mux := grantMux(&fakeGrants{})
	for name, c := range map[string]struct{ method, path string }{
		"bad agent id":   {"GET", "/api/v1/agents/bob/grants?database=db1&limit=5"},
		"no database":    {"GET", "/api/v1/agents/" + testAgent + "/grants?limit=5"},
		"bad database":   {"GET", "/api/v1/agents/" + testAgent + "/grants?database=a;b"},
		"bad limit":      {"GET", "/api/v1/agents/" + testAgent + "/grants?database=d&limit=x"},
		"bad grant id":   {"POST", "/api/v1/agents/" + testAgent + "/grants/x/revoke"},
		"zero grant id":  {"POST", "/api/v1/agents/" + testAgent + "/grants/0/revoke"},
		"bad request id": {"POST", "/api/v1/agents/" + testAgent + "/grant-requests/-1/deny"},
	} {
		code, _ := doJSON(t, mux, operatorUser(), c.method, c.path,
			map[string]any{"database": "db1"})
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, code)
		}
	}
	code, _ := doJSON(t, mux, operatorUser(), "POST", "/api/v1/agents/"+testAgent+"/grants",
		map[string]any{"database": "db1", "unknown": 1})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field: %d", code)
	}
	code, _ = doJSON(t, mux, operatorUser(), "POST",
		"/api/v1/agents/"+testAgent+"/grants/7/revoke", map[string]any{})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("revoke without database: %d", code)
	}
}

func TestAgentGrantRoutes_NoServiceIs503(t *testing.T) {
	mux := grantMux(nil)
	code, body := doJSON(t, mux, operatorUser(), "GET",
		"/api/v1/agents/"+testAgent+"/grants?database=db1&limit=5", nil)
	if code != http.StatusServiceUnavailable || body["code"] != "unavailable" {
		t.Fatalf("no service: %d %v", code, body)
	}
}
