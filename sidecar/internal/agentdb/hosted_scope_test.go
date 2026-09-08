package agentdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostedSupabasePreflightRejectsMissingBranchEntitlement(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		switch r.URL.Path {
		case "/projects/owned-project":
			fmt.Fprint(w, `{"ref":"owned-project","organization_slug":"owned-org"}`)
		case "/organizations/owned-org/entitlements":
			fmt.Fprint(w, `{"entitlements":[{"feature":{"key":"branching_limit"},
			"hasAccess":false,"config":{"enabled":false,"value":0}}]}`)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := HostedHTTPClient{Provider: "supabase", BaseURL: server.URL,
		TokenFunc: staticToken("synthetic-token")}
	runner := NewHostedRunner("supabase", client)
	req := hostedRequest("supabase")
	if got := runner.Preflight(context.Background(), req); got.Status != "preflight_failed" {
		t.Fatalf("missing entitlement passed preflight: %+v", got)
	}
	if got := runner.Create(context.Background(), req); got.Error == nil || posts != 0 {
		t.Fatalf("unauthorized branch POST: %+v posts=%d", got, posts)
	}
}
