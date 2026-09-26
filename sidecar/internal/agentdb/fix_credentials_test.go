package agentdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// G8-B19: Supabase projects never share one database password.
func TestSupabaseProjectPasswordsAreDistinctPerProject(t *testing.T) {
	client := HostedHTTPClient{Provider: ProviderSupabase, PasswordFunc: staticToken("master")}
	passwords := map[string]string{}
	for _, name := range []string{"pgsage-a-1", "pgsage-b-2"} {
		body, err := client.supabaseCreateBody(context.Background(), HostedCreateInput{
			Mode: "project", Scope: "org", Name: name, Region: "us-east-1"})
		if err != nil {
			t.Fatalf("create body: %v", err)
		}
		pass, _ := body["db_pass"].(string)
		if pass == "" || pass == "master" || len(pass) < 24 {
			t.Fatalf("project %s password not derived per project: %q", name, pass)
		}
		passwords[name] = pass
	}
	if passwords["pgsage-a-1"] == passwords["pgsage-b-2"] {
		t.Fatal("two Supabase projects share a database password")
	}
}

// G8-B09/SURF-20: an expired or rejected Cloud SQL token surfaces as a
// typed permission error instead of an opaque failure.
func TestCloudSQLUnauthorizedIsTypedPermissionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Request had invalid credentials"}}`))
	}))
	defer server.Close()
	client := CloudSQLHTTPClient{BaseURL: server.URL, TokenFunc: staticToken("expired")}
	err := client.DeleteInstance(context.Background(), "proj", "pgsage-x")
	var pe ProviderError
	if !errors.As(err, &pe) || pe.Kind != ProviderErrPermission {
		t.Fatalf("401 error = %#v, want typed permission error", err)
	}
}

// G8-B14: a secret_ref may only name an allow-listed environment variable,
// never the meta-database DSN.
func TestRegisterRejectsSecretRefOutsideAllowlist(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_secret_ref"
	cleanupDeployment(t, ctx, pool, id)
	_, err := st.Register(ctx, RegisterRequest{DeploymentID: id, TenantID: "tenant_agentdb_test",
		AgentID: "agent_secret", IsolationType: "schema", SecretRef: "env:SAGE_DATABASE_URL"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("meta DSN secret_ref err = %v, want ErrInvalid", err)
	}
	dep, err := st.Register(ctx, RegisterRequest{DeploymentID: id, TenantID: "tenant_agentdb_test",
		AgentID: "agent_secret", IsolationType: "schema",
		SecretRef: "env:PG_SAGE_AGENTDB_ORDERS_DSN"})
	if err != nil || dep.SecretRef != "env:PG_SAGE_AGENTDB_ORDERS_DSN" {
		t.Fatalf("allow-listed secret_ref: dep=%q err=%v", dep.SecretRef, err)
	}
}
