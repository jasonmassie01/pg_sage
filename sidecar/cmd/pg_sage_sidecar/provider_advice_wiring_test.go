package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/config"
)

const adviceSubscription = "00000000-0000-0000-0000-0000000000aa"

func adviceDeps(tokenErr error) cloudDeps {
	return cloudDeps{azureToken: func() (azure.TokenSource, error) {
		if tokenErr != nil {
			return nil, tokenErr
		}
		return func(context.Context) (string, error) { return "test-token", nil }, nil
	}}
}

func adviceConfig(enabled *bool) *config.Config {
	return &config.Config{ProviderAdvice: config.ProviderAdviceConfig{Enabled: enabled},
		Azure: config.AzureConfig{SubscriptionID: adviceSubscription, ResourceGroup: "rg"}}
}

func providersOf(t *testing.T, c *config.Config, target adviceTarget, deps cloudDeps,
	token string) ([]string, string) {
	t.Helper()
	sources, note := providerAdviceSources(c, target, deps, token)
	var names []string
	for _, s := range sources {
		names = append(names, s.Provider())
	}
	return names, note
}

func TestProviderAdviceSources_PerProvider(t *testing.T) {
	pool := lazyPool(t, "app")
	supaHost := "db.abcdefghijklmnopqrst.supabase.co"
	cases := []struct {
		name, provider, host, token, want, note string
	}{
		{"cloud sql", "cloud-sql", "10.0.0.5", "", "cloud-sql", ""},
		{"alloydb", "alloydb", "10.0.0.6", "", "alloydb", ""},
		{"azure", "azure", "pg-app.postgres.database.azure.com", "", "azure", ""},
		{"supabase", "supabase", supaHost, "tok", "supabase", ""},
		{"supabase no token", "supabase", supaHost, "", "", "SAGE_SUPABASE_OBSERVABILITY_TOKEN"},
		{"neon", "neon", "ep-x.us-east-2.aws.neon.tech", "", "", "no advisor API"},
		{"rds", "rds", "db.abc.us-east-1.rds.amazonaws.com", "", "", ""},
		{"self-managed", "", "10.0.0.9", "tok", "", ""},
	}
	for _, tc := range cases {
		names, note := providersOf(t, adviceConfig(nil), adviceTarget{provider: tc.provider,
			host: tc.host, user: "postgres", database: "app", pool: pool}, adviceDeps(nil),
			tc.token)
		if strings.Join(names, ",") != tc.want {
			t.Errorf("%s: sources %v, want %q (note %q)", tc.name, names, tc.want, note)
		}
		if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
			t.Errorf("%s: note %q, want %q", tc.name, note, tc.note)
		}
	}
}

func TestProviderAdviceSources_DisabledOrNoPoolIsNothing(t *testing.T) {
	off := false
	target := adviceTarget{provider: "cloud-sql", host: "10.0.0.5", database: "app",
		pool: lazyPool(t, "app")}
	names, note := providersOf(t, adviceConfig(&off), target, adviceDeps(nil), "")
	if len(names) != 0 || note != "" {
		t.Fatalf("disabled: %v %q", names, note)
	}
	target.pool = nil
	if names, _ := providersOf(t, adviceConfig(nil), target, adviceDeps(nil), ""); len(names) != 0 {
		t.Fatalf("no pool: %v", names)
	}
	if names, _ := providersOf(t, nil, target, adviceDeps(nil), ""); len(names) != 0 {
		t.Fatalf("nil config: %v", names)
	}
}

func TestProviderAdviceSources_AzureSaysWhatIsMissing(t *testing.T) {
	pool := lazyPool(t, "app")
	target := adviceTarget{provider: "azure", host: "10.1.2.3", database: "app", pool: pool}
	_, note := providersOf(t, adviceConfig(nil), target, adviceDeps(nil), "")
	if !strings.Contains(note, "azure.server_name") {
		t.Fatalf("no server: note %q", note)
	}
	c := adviceConfig(nil)
	c.Azure.SubscriptionID = ""
	target.host = "pg-app.postgres.database.azure.com"
	if _, note = providersOf(t, c, target, adviceDeps(nil), ""); !strings.Contains(note,
		"azure.subscription_id") {
		t.Fatalf("no subscription: note %q", note)
	}
	names, note := providersOf(t, adviceConfig(nil), target,
		adviceDeps(errors.New("no managed identity")), "")
	if len(names) != 0 || !strings.Contains(note, "no managed identity") {
		t.Fatalf("no credential: %v %q", names, note)
	}
	target.azure = config.CloudResourceAzure{Subscription: adviceSubscription,
		ResourceGroup: "rg-db", Server: "pg-other"}
	target.host = "10.1.2.3"
	names, note = providersOf(t, &config.Config{}, target, adviceDeps(nil), "")
	if len(names) != 1 || note != "" {
		t.Fatalf("cloud_resource.azure names the server: %v %q", names, note)
	}
}

// Azure index tuning filters by the PostgreSQL database name, so the
// target must carry the pool's dbname, not the fleet instance name.
func TestAdviceTarget_NamesThePostgresDatabaseNotTheInstance(t *testing.T) {
	rt := &databaseRuntime{provider: "azure", spec: databaseRuntimeSpec{
		Name: "orders-prod", Pool: lazyPool(t, "orders")}}
	got := rt.adviceTarget()
	if got.database != "orders" || got.user != "u" || got.host != "127.0.0.1" ||
		got.provider != "azure" || got.pool != rt.spec.Pool {
		t.Fatalf("target = %+v, want database orders of 127.0.0.1", got)
	}
	if db := postgresDatabaseName("", "app_owner"); db != "app_owner" {
		t.Fatalf("no dbname: %q, want the user name (the server default)", db)
	}
	if db := postgresDatabaseName("orders", "app_owner"); db != "orders" {
		t.Fatalf("dbname: %q, want orders", db)
	}
}
