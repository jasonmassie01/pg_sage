package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
)

// GET /api/v1/agents/freezes (agentgov-07): operators and admins read the
// open fleet, database and agent freezes with each one's pending release,
// so the Agents page can show a kill's flag and lift it.

func (f *fakeKillSwitch) OpenFreezes(context.Context) (agentguard.FreezeList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.freezeList, f.listErr
}

const freezesPath = "/api/v1/agents/freezes"

func sampleFreezes() agentguard.FreezeList {
	set := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	return agentguard.FreezeList{Items: []agentguard.ListedFreeze{
		{ID: 7, Scope: "fleet", Reason: "runaway agent", SetBy: "a@example.com", SetAt: set,
			AfterKill: true, PendingRelease: &agentguard.PendingRelease{RequestID: 3,
				RequestedBy: "a@example.com", RequestedAt: set.Add(time.Minute),
				ExpiresAt: set.Add(16 * time.Minute)}},
		{ID: 8, Scope: "database", Target: "orders", Reason: "drill", SetBy: "b@example.com",
			SetAt: set},
	}}
}

func TestAgentFreezesRoute_Roles(t *testing.T) {
	mux := newKillAPI(&fakeKillSwitch{freezeList: sampleFreezes()})
	users := [4]*auth.User{nil, viewerUser(), operatorUser(), adminNamed("a@example.com")}
	want := [4]int{http.StatusUnauthorized, http.StatusForbidden, http.StatusOK,
		http.StatusOK}
	for i, u := range users {
		if code, body := doJSON(t, mux, u, "GET", freezesPath, nil); code != want[i] {
			t.Errorf("as %v: %d %v, want %d", u, code, body, want[i])
		}
	}
}

func TestAgentFreezesRoute_Body(t *testing.T) {
	mux := newKillAPI(&fakeKillSwitch{freezeList: sampleFreezes()})
	code, body := doJSON(t, mux, operatorUser(), "GET", freezesPath, nil)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	if body["truncated"] != false {
		t.Fatalf("truncated = %v", body["truncated"])
	}
	items, ok := body["freezes"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("freezes = %v", body["freezes"])
	}
	fleet := items[0].(map[string]any)
	for k, v := range map[string]any{"scope": "fleet", "target": "", "reason": "runaway agent",
		"set_by": "a@example.com", "after_kill": true, "id": float64(7)} {
		if fleet[k] != v {
			t.Errorf("fleet %s = %v, want %v (%v)", k, fleet[k], v, fleet)
		}
	}
	if fleet["set_at"] != "2026-11-11T09:00:00Z" {
		t.Errorf("set_at = %v", fleet["set_at"])
	}
	pend, ok := fleet["pending_release"].(map[string]any)
	if !ok || pend["request_id"] != float64(3) || pend["requested_by"] != "a@example.com" ||
		pend["expires_at"] != "2026-11-11T09:16:00Z" {
		t.Fatalf("pending_release = %v", fleet["pending_release"])
	}
	db := items[1].(map[string]any)
	if db["target"] != "orders" || db["after_kill"] != false || db["pending_release"] != nil {
		t.Fatalf("database freeze = %v", db)
	}
}

func TestAgentFreezesRoute_EmptyIsAList(t *testing.T) {
	// nil/empty: no open freeze is an empty array, never null.
	mux := newKillAPI(&fakeKillSwitch{})
	code, body := doJSON(t, mux, adminNamed("a@example.com"), "GET", freezesPath, nil)
	items, ok := body["freezes"].([]any)
	if code != http.StatusOK || !ok || len(items) != 0 || body["truncated"] != false {
		t.Fatalf("empty: %d %v", code, body)
	}
}

func TestAgentFreezesRoute_Truncated(t *testing.T) {
	list := sampleFreezes()
	list.Truncated = true
	mux := newKillAPI(&fakeKillSwitch{freezeList: list})
	_, body := doJSON(t, mux, adminNamed("a@example.com"), "GET", freezesPath, nil)
	if body["truncated"] != true {
		t.Fatalf("truncated = %v", body["truncated"])
	}
}

func TestAgentFreezesRoute_Errors(t *testing.T) {
	cases := []struct {
		name string
		sw   AgentKillSwitch
		want int
		code string
	}{
		{"no switch", nil, http.StatusServiceUnavailable, "unavailable"},
		{"no control database", &fakeKillSwitch{listErr: agentguard.ErrUnavailable},
			http.StatusServiceUnavailable, "unavailable"},
		{"storage", &fakeKillSwitch{listErr: fmt.Errorf("agentguard: listing freezes: boom")},
			http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		code, body := doJSON(t, newKillAPI(tc.sw), operatorUser(), "GET", freezesPath, nil)
		if code != tc.want {
			t.Errorf("%s: %d %v, want %d", tc.name, code, body, tc.want)
			continue
		}
		if tc.code != "" && body["code"] != tc.code {
			t.Errorf("%s: code %v, want %s", tc.name, body["code"], tc.code)
		}
		if msg, _ := body["error"].(string); tc.code == "" && strings.Contains(msg, "boom") {
			t.Errorf("%s: a storage error leaks its detail: %q", tc.name, msg)
		}
	}
}

func TestAgentFreezesRoute_DoesNotShadowAgentGet(t *testing.T) {
	// boundary: the literal freezes path wins over GET /api/v1/agents/{id},
	// and POST to it is not a route.
	mux := newKillAPI(&fakeKillSwitch{})
	code, _ := doJSON(t, mux, adminNamed("a@example.com"), "POST", freezesPath, nil)
	if code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
		t.Fatalf("POST freezes: %d", code)
	}
}
