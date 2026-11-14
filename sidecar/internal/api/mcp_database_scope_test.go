package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// apiauth-08c: a person's MCP principal (KindHuman, an operator token) is
// narrowed at each call to the token's databases intersected with the
// owner's visible databases. Agent principals keep their own lists.

func humanPrincipal(owner int, role string, dbs []string) mcp.Principal {
	return mcp.Principal{Actor: "token:t", Role: role, Kind: mcp.KindHuman,
		Databases: dbs, OwnerUserID: owner, TokenID: "t"}
}

func scopeDatabasesText(dbs []string) string {
	if dbs == nil {
		return "<all>"
	}
	return "[" + strings.Join(dbs, ",") + "]"
}

func TestScopeMCPPrincipal_Rules(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	src.set(8, map[string]string{"billing": auth.RoleOperator, "search": auth.RoleViewer})
	a := &databaseAuthz{grants: src}
	cases := []struct {
		name  string
		authz *databaseAuthz
		p     mcp.Principal
		want  string
	}{
		{"switch off keeps the token list", nil,
			humanPrincipal(7, auth.RoleOperator, nil), "<all>"},
		{"star token of a scoped owner lists the grants", a,
			humanPrincipal(7, auth.RoleOperator, nil), "[billing]"},
		{"two grants, sorted", a, humanPrincipal(8, auth.RoleViewer, nil), "[billing,search]"},
		{"token list intersected", a,
			humanPrincipal(8, auth.RoleOperator, []string{"orders", "search"}), "[search]"},
		{"token outside the grants reads nothing", a,
			humanPrincipal(7, auth.RoleOperator, []string{"search"}), "[]"},
		{"unscoped owner keeps the token list", a,
			humanPrincipal(9, auth.RoleOperator, []string{"orders"}), "[orders]"},
		{"unscoped owner star token", a, humanPrincipal(9, auth.RoleOperator, nil), "<all>"},
		{"admin owner is never scoped", a, humanPrincipal(7, auth.RoleAdmin, nil), "<all>"},
		{"legacy owner-less token is fleet-only with grants on", a,
			humanPrincipal(0, auth.RoleOperator, nil), "[]"},
		{"legacy owner-less token unchanged with grants off", nil,
			humanPrincipal(0, auth.RoleOperator, nil), "<all>"},
		{"unknown owner role fails closed", a, humanPrincipal(7, "superuser", nil), "[]"},
		{"agent keeps its own list", a, mcp.Principal{Actor: "token:a", Kind: mcp.KindAgent,
			Databases: []string{"search"}, OwnerUserID: 7}, "[search]"},
	}
	for _, tc := range cases {
		got, err := scopeMCPPrincipal(context.Background(), tc.authz, tc.p)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if s := scopeDatabasesText(got.Databases); s != tc.want {
			t.Errorf("%s: databases %s, want %s", tc.name, s, tc.want)
		}
		if got.DatabasesUnavailable {
			t.Errorf("%s: marked unavailable", tc.name)
		}
		if got.Actor != tc.p.Actor || got.Role != tc.p.Role || got.Kind != tc.p.Kind {
			t.Errorf("%s: identity changed: %+v", tc.name, got)
		}
	}
}

func TestScopeMCPPrincipal_AgentAndAdminReadNoGrants(t *testing.T) {
	src := &dbAuthzSource{err: errors.New("must not be read")}
	a := &databaseAuthz{grants: src}
	for _, p := range []mcp.Principal{
		{Actor: "token:a", Kind: mcp.KindAgent, Databases: []string{"search"}},
		humanPrincipal(7, auth.RoleAdmin, nil),
		humanPrincipal(0, auth.RoleOperator, nil),
	} {
		if _, err := scopeMCPPrincipal(context.Background(), a, p); err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
	}
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("grant store read %d times, want 0", n)
	}
}

func TestScopeMCPPrincipal_StoreFailureNeverWidens(t *testing.T) {
	cause := errors.New("connection refused")
	a := &databaseAuthz{grants: &dbAuthzSource{err: cause}}
	got, err := scopeMCPPrincipal(context.Background(), a,
		humanPrincipal(7, auth.RoleOperator, nil))
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want the store's", err)
	}
	if got.Databases == nil || len(got.Databases) != 0 || !got.DatabasesUnavailable {
		t.Fatalf("on failure: databases %s unavailable=%v, want [] and true",
			scopeDatabasesText(got.Databases), got.DatabasesUnavailable)
	}
}

// State transitions: a grant added after mint widens the token up to its
// own list, never beyond; revoking narrows the next call.
func TestScopeMCPPrincipal_GrantChangesApplyAtTheNextCall(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	a := &databaseAuthz{grants: src}
	p := humanPrincipal(7, auth.RoleOperator, []string{"billing", "search"})
	step := func(want string) {
		t.Helper()
		got, err := scopeMCPPrincipal(context.Background(), a, p)
		if err != nil {
			t.Fatal(err)
		}
		if s := scopeDatabasesText(got.Databases); s != want {
			t.Fatalf("databases %s, want %s", s, want)
		}
	}
	step("[billing]")
	src.set(7, map[string]string{"billing": auth.RoleViewer, "search": auth.RoleViewer})
	step("[billing,search]")
	src.set(7, map[string]string{"billing": auth.RoleViewer, "search": auth.RoleViewer,
		"orders": auth.RoleOperator})
	step("[billing,search]")
	src.set(7, map[string]string{"search": auth.RoleViewer})
	step("[search]")
	if p.Databases[0] != "billing" || len(p.Databases) != 2 {
		t.Fatalf("the caller's principal was modified: %v", p.Databases)
	}
}

func TestScopeMCPPrincipal_ParallelOwnersSeeTheirOwnSets(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	src.set(8, map[string]string{"search": auth.RoleViewer})
	a := &databaseAuthz{grants: src}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		owner, want := 7, "[billing]"
		if i%2 == 1 {
			owner, want = 8, "[search]"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := scopeMCPPrincipal(context.Background(), a,
				humanPrincipal(owner, auth.RoleOperator, nil))
			if err != nil || scopeDatabasesText(got.Databases) != want {
				errs <- fmt.Errorf("owner %d: %s %v, want %s", owner,
					scopeDatabasesText(got.Databases), err, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Error propagation: without a control database the owner cannot be
// checked, so the mint is refused (503), never allowed unchecked.
func TestMintWithinOwnerGrants_NoPoolIsUnavailable(t *testing.T) {
	a := &databaseAuthz{grants: &dbAuthzSource{}}
	req := httptest.NewRequest(http.MethodPost, tokenRoutesPath, nil)
	req = req.WithContext(context.WithValue(req.Context(), databaseAuthzKey, a))
	body := createMCPTokenBody{Kind: "operator", OwnerUserID: 5, Databases: []string{"*"}}
	w := httptest.NewRecorder()
	if mintWithinOwnerGrants(w, req, nil, body) {
		t.Fatal("mint allowed without a control database")
	}
	if w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), "grant_store_unavailable") {
		t.Fatalf("got %d %s, want 503 grant_store_unavailable", w.Code, w.Body)
	}
	// Grants off, an agent token or no owner: nothing to check.
	for _, tc := range []struct {
		ctx  context.Context
		body createMCPTokenBody
	}{
		{context.Background(), body},
		{req.Context(), createMCPTokenBody{Kind: "agent", Databases: []string{"*"}}},
		{req.Context(), createMCPTokenBody{Kind: "operator", Databases: []string{"*"}}},
	} {
		w := httptest.NewRecorder()
		if !mintWithinOwnerGrants(w, req.WithContext(tc.ctx), nil, tc.body) {
			t.Fatalf("%+v refused: %d %s", tc.body, w.Code, w.Body)
		}
	}
}
