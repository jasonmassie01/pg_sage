package agentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func hostedHTTPCreateCases() []struct{ provider, mode, path, response string } {
	return []struct{ provider, mode, path, response string }{
		{"neon", "branch", "/projects/owned-project/branches",
			`{"branch":{"id":"br-created","project_id":"owned-project",
			"name":"pgsage-test","current_state":"ready"},
			"endpoints":[{"host":"ep-test.neon.tech","current_state":"active"}]}`},
		{"neon", "project", "/projects",
			`{"project":{"id":"project-created","org_id":"owned-org",
			"name":"pgsage-test"},"endpoints":[{"host":"ep-test.neon.tech",
			"current_state":"active"}]}`},
		{"supabase", "branch", "/projects/owned-project/branches",
			`{"id":"branch-created","name":"pgsage-test",
			"parent_project_ref":"owned-project","project_ref":"child-project",
			"is_default":false,"preview_project_status":"COMING_UP"}`},
		{"supabase", "project", "/projects",
			`{"ref":"project-created","organization_slug":"owned-org",
			"name":"pgsage-test","status":"COMING_UP"}`},
	}
}

func TestHostedHTTPCreateWireContracts(t *testing.T) {
	for _, tc := range hostedHTTPCreateCases() {
		t.Run(tc.provider+"/"+tc.mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != tc.path ||
					r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Errorf("unexpected request method/path/auth")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				assertHostedCreatePayload(t, tc.provider, tc.mode, payload)
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			client := HostedHTTPClient{Provider: tc.provider, BaseURL: server.URL,
				TokenFunc: staticToken("synthetic-token"), PasswordFunc: staticToken("synthetic-password")}
			scope := "owned-project"
			if tc.mode == "project" {
				scope = "owned-org"
			}
			got, err := client.CreateResource(context.Background(), HostedCreateInput{
				Mode: tc.mode, Scope: scope, Name: "pgsage-test", Source: "source-id", Region: "us-east-1",
			})
			if err != nil || got.ID == "" || got.Name != "pgsage-test" || calls != 1 {
				t.Fatalf("create result=%+v err=%v calls=%d", got, err, calls)
			}
		})
	}
}

func assertHostedCreatePayload(t *testing.T, provider, mode string, payload map[string]any) {
	t.Helper()
	if provider == "neon" {
		resource, _ := payload[mode].(map[string]any)
		if resource["name"] != "pgsage-test" {
			t.Fatal("Neon name missing")
		}
		if mode == "branch" {
			if resource["init_source"] != "parent-schema" || resource["parent_id"] != "source-id" {
				t.Fatal("Neon source/schema-only request missing")
			}
			endpoints, _ := payload["endpoints"].([]any)
			if len(endpoints) != 1 || endpoints[0].(map[string]any)["type"] != "read_write" {
				t.Fatal("Neon branch missing compute")
			}
			if _, exists := endpoints[0].(map[string]any)["suspend_timeout_seconds"]; exists {
				t.Fatal("Neon Free disallows changing the provider-managed suspend interval")
			}
		} else {
			settings, _ := resource["default_endpoint_settings"].(map[string]any)
			if _, exists := settings["suspend_timeout_seconds"]; exists {
				t.Fatal("Neon Free project must retain provider-default suspend interval")
			}
		}
		return
	}
	if mode == "project" {
		if payload["organization_slug"] != "owned-org" || payload["db_pass"] != "synthetic-password" {
			t.Fatal("Supabase organization/password missing")
		}
		if _, ok := payload["plan"]; ok {
			t.Fatal("deprecated plan field is ineffective")
		}
	} else if payload["branch_name"] != "pgsage-test" || payload["with_data"] != false {
		t.Fatal("Supabase schema-only branch payload missing")
	}
}

func TestHostedHTTPRejectsMalformedAndSensitiveErrors(t *testing.T) {
	for _, code := range []int{200, 401, 402, 403, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, "synthetic-private-password not valid JSON")
			}))
			defer server.Close()
			client := HostedHTTPClient{Provider: "neon", BaseURL: server.URL,
				TokenFunc: staticToken("synthetic-token")}
			got, err := client.GetResource(context.Background(), "branch", "project", "br-id")
			if err == nil || got.ID != "" || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatalf("invalid or leaking API outcome: %+v %v", got, err)
			}
		})
	}
}

func TestHostedHTTPDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls++
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := HostedHTTPClient{Provider: "neon", BaseURL: source.URL,
		TokenFunc: staticToken("synthetic-token")}
	_, err := client.GetResource(context.Background(), "project", "org", "project")
	if err == nil || destinationCalls != 0 {
		t.Fatal("redirect reached another origin")
	}
}

func TestHostedHTTPRequiresCredentialsBeforeRequest(t *testing.T) {
	client := HostedHTTPClient{Provider: "supabase"}
	_, err := client.CreateResource(context.Background(), HostedCreateInput{Mode: "project"})
	if err == nil {
		t.Fatal("missing credentials accepted")
	}
}

func TestHostedSupabaseProjectRequiresRegion(t *testing.T) {
	client := HostedHTTPClient{Provider: "supabase", TokenFunc: staticToken("synthetic-token"),
		PasswordFunc: staticToken("synthetic-password")}
	_, err := client.supabaseCreateBody(context.Background(), HostedCreateInput{
		Mode: "project", Name: "test", Scope: "org",
	})
	if err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("missing region must fail before API: %v", err)
	}
}

func TestHostedHTTPGetDoesNotSendJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 || r.Header.Get("Content-Type") != "" {
			t.Error("GET requests must not send JSON null or a JSON content type")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"ref":"project","name":"test","organization_slug":"org",
		"status":"ACTIVE_HEALTHY"}`)
	}))
	defer server.Close()
	client := HostedHTTPClient{Provider: "supabase", BaseURL: server.URL,
		TokenFunc: staticToken("synthetic-token")}
	resource, err := client.GetResource(context.Background(), "project", "org", "project")
	if err != nil || resource.State != "available" {
		t.Fatalf("GET result: %+v %v", resource, err)
	}
}
