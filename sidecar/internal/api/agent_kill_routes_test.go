package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Spec §8.3: POST /api/v1/agents/kill (admin) → 200 with the per-database
// report; POST /api/v1/agents/{id}/freeze (operator) → 200; POST
// /api/v1/agents/{id}/unfreeze (admin; two people after a kill) → 202;
// POST /api/v1/agents/kill/release (admin; two people) → 202. The actor is
// always the session user, never the body.

const killTestPID = "agp_aaaaaaaaaaaaaaaaaaaa"

type fakeKillSwitch struct {
	mu        sync.Mutex
	kills     []agentguard.KillRequest
	freezes   []agentguard.FreezeRequest
	unfreezes []agentguard.UnfreezeRequest
	releases  []agentguard.ReleaseRequest
	err       error
	unfreeze  agentguard.UnfreezeResult
}

func (f *fakeKillSwitch) Kill(_ context.Context, r agentguard.KillRequest) (
	agentguard.KillReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, r)
	if err := r.Validate(); err != nil {
		return agentguard.KillReport{}, err
	}
	return agentguard.KillReport{KillID: 9, Scope: r.Scope, Verified: true,
		Databases: []agentguard.DatabaseReport{{Name: "orders", RolesDisabled: 2,
			BackendsTerminated: 1, Verified: true,
			Replicas: []agentguard.ReplicaReport{{Name: "standby2", Configured: false,
				Bound: &agentguard.SessionBound{StatementTimeoutMS: 30000}}}}}}, f.err
}

func (f *fakeKillSwitch) Freeze(_ context.Context, r agentguard.FreezeRequest) (
	agentguard.KillReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freezes = append(f.freezes, r)
	if err := r.Validate(); err != nil {
		return agentguard.KillReport{}, err
	}
	return agentguard.KillReport{Scope: agentguard.KillScopePrincipal, Verified: true,
		Principals: []string{r.PrincipalID}}, f.err
}

func (f *fakeKillSwitch) Unfreeze(_ context.Context, r agentguard.UnfreezeRequest) (
	agentguard.UnfreezeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unfreezes = append(f.unfreezes, r)
	if err := r.Validate(); err != nil {
		return agentguard.UnfreezeResult{}, err
	}
	return f.unfreeze, f.err
}

func (f *fakeKillSwitch) Release(_ context.Context, r agentguard.ReleaseRequest) (
	agentguard.UnfreezeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, r)
	if err := r.Validate(); err != nil {
		return agentguard.UnfreezeResult{}, err
	}
	return f.unfreeze, f.err
}

func newKillAPI(sw AgentKillSwitch) *http.ServeMux {
	mux := http.NewServeMux()
	registerAgentKillRoutes(mux, sw)
	return mux
}

func TestAgentKillRoutes_Roles(t *testing.T) {
	sw := &fakeKillSwitch{}
	mux := newKillAPI(sw)
	kill := map[string]any{"scope": "all", "reason": "incident"}
	cases := []struct {
		path string
		body any
		// codes for anonymous, viewer, operator, admin
		want [4]int
	}{
		{"/api/v1/agents/kill", kill, [4]int{401, 403, 403, 200}},
		{"/api/v1/agents/" + killTestPID + "/freeze", map[string]any{"reason": "r"},
			[4]int{401, 403, 200, 200}},
		{"/api/v1/agents/" + killTestPID + "/unfreeze", map[string]any{"reason": "r"},
			[4]int{401, 403, 403, 202}},
		{"/api/v1/agents/kill/release", map[string]any{"scope": "all", "reason": "r"},
			[4]int{401, 403, 403, 202}},
	}
	users := [4]*auth.User{nil, viewerUser(), operatorUser(), adminNamed("a@example.com")}
	for _, tc := range cases {
		for i, u := range users {
			code, _ := doJSON(t, mux, u, "POST", tc.path, tc.body)
			if code != tc.want[i] {
				t.Errorf("%s as %v: %d, want %d", tc.path, u, code, tc.want[i])
			}
		}
	}
}

func TestAgentKillRoutes_KillReportAndActor(t *testing.T) {
	sw := &fakeKillSwitch{}
	mux := newKillAPI(sw)
	code, body := doJSON(t, mux, adminNamed("boss@example.com"), "POST", "/api/v1/agents/kill",
		map[string]any{"scope": "principal", "id": killTestPID, "reason": "incident 7",
			"actor": "someone-else@example.com"})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown body field must be refused: %d %v", code, body)
	}
	code, body = doJSON(t, mux, adminNamed("boss@example.com"), "POST", "/api/v1/agents/kill",
		map[string]any{"scope": "principal", "id": killTestPID, "reason": "incident 7"})
	if code != http.StatusOK {
		t.Fatalf("kill: %d %v", code, body)
	}
	last := sw.kills[len(sw.kills)-1]
	if last.Actor != "boss@example.com" || last.ID != killTestPID || last.Reason != "incident 7" {
		t.Fatalf("request = %+v", last)
	}
	dbs, ok := body["databases"].([]any)
	if !ok || len(dbs) != 1 {
		t.Fatalf("databases = %v", body["databases"])
	}
	db := dbs[0].(map[string]any)
	for _, k := range []string{"name", "roles_disabled", "backends_terminated", "replicas",
		"verified"} {
		if _, ok := db[k]; !ok {
			t.Fatalf("database report lacks %q: %v", k, db)
		}
	}
	rep := db["replicas"].([]any)[0].(map[string]any)
	if rep["configured"] != false || rep["bound"] == nil {
		t.Fatalf("unconfigured standby = %v", rep)
	}
}

func TestAgentKillRoutes_Errors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"invalid", fmt.Errorf("%w: x", agentguard.ErrInvalid), 422, "invalid_arguments"},
		{"not found", fmt.Errorf("%w: x", agentguard.ErrNotFound), 404, "not_found"},
		{"unavailable", agentguard.ErrUnavailable, 503, "unavailable"},
		{"not frozen", agentguard.ErrNotFrozen, 409, "not_frozen"},
		{"sponsor", agentguard.ErrSponsorCannotApprove, 403, "sponsor_cannot_approve"},
		{"withheld", &executor.WithheldError{Decision: executor.ActionPolicyDecision{
			Decision: executor.PolicyDecisionBlocked, BlockedReason: "emergency_stop"}},
			409, "emergency_stop"},
		{"other", fmt.Errorf("boom"), 500, ""},
	}
	for _, tc := range cases {
		sw := &fakeKillSwitch{err: tc.err}
		mux := newKillAPI(sw)
		code, body := doJSON(t, mux, adminNamed("a@example.com"), "POST",
			"/api/v1/agents/"+killTestPID+"/unfreeze", map[string]any{"reason": "r"})
		if code != tc.want {
			t.Errorf("%s: %d %v, want %d", tc.name, code, body, tc.want)
			continue
		}
		if tc.code == "" {
			continue
		}
		got, _ := body["code"].(string)
		if reason, _ := body["reason_code"].(string); reason != "" {
			got = reason
		}
		if got != tc.code {
			t.Errorf("%s: code %q, want %q (%v)", tc.name, got, tc.code, body)
		}
	}
}

func TestAgentKillRoutes_BadInput(t *testing.T) {
	mux := newKillAPI(&fakeKillSwitch{})
	a := adminNamed("a@example.com")
	for name, tc := range map[string]struct {
		path string
		body any
	}{
		"kill not json":     {"/api/v1/agents/kill", "nope"},
		"kill bad scope":    {"/api/v1/agents/kill", map[string]any{"scope": "x", "reason": "r"}},
		"kill no reason":    {"/api/v1/agents/kill", map[string]any{"scope": "all"}},
		"freeze bad id":     {"/api/v1/agents/bot-1/freeze", map[string]any{"reason": "r"}},
		"unfreeze extra":    {"/api/v1/agents/" + killTestPID + "/unfreeze", map[string]any{"x": 1}},
		"release principal": {"/api/v1/agents/kill/release", map[string]any{"scope": "principal"}},
	} {
		code, body := doJSON(t, mux, a, "POST", tc.path, tc.body)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %v, want 422", name, code, body)
		}
	}
}

func TestAgentKillRoutes_UnfreezePendingAndApplied(t *testing.T) {
	sw := &fakeKillSwitch{unfreeze: agentguard.UnfreezeResult{Pending: true, RequestID: 4,
		RequestedBy: "a@example.com", Quorum: 2}}
	mux := newKillAPI(sw)
	user := adminNamed("a@example.com")
	code, body := doJSON(t, mux, user, "POST", "/api/v1/agents/"+killTestPID+"/unfreeze",
		map[string]any{"reason": "closed"})
	if code != http.StatusAccepted || body["pending"] != true || body["quorum"] != float64(2) {
		t.Fatalf("pending: %d %v", code, body)
	}
	got := sw.unfreezes[0]
	if got.ActorUserID != user.ID || got.Actor != "a@example.com" || got.Reason != "closed" {
		t.Fatalf("request = %+v", got)
	}
	sw.unfreeze = agentguard.UnfreezeResult{Applied: true, Quorum: 2}
	code, body = doJSON(t, mux, adminNamed("b@example.com"), "POST",
		"/api/v1/agents/"+killTestPID+"/unfreeze", map[string]any{"reason": "closed"})
	if code != http.StatusAccepted || body["applied"] != true {
		t.Fatalf("applied: %d %v", code, body)
	}
}

func TestAgentKillRoutes_NoSwitch(t *testing.T) {
	mux := newKillAPI(nil)
	code, body := doJSON(t, mux, adminNamed("a@example.com"), "POST", "/api/v1/agents/kill",
		map[string]any{"scope": "all", "reason": "r"})
	if code != http.StatusServiceUnavailable || body["code"] != "unavailable" {
		t.Fatalf("no switch: %d %v", code, body)
	}
}
