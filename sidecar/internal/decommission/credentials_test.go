package decommission

import (
	"strings"
	"testing"
)

func TestCredentials_ListsEveryProviderSecretAndGrantWithoutValues(t *testing.T) {
	set := map[string]string{
		"PG_SAGE_SUPABASE_DATABASE_PASSWORD": "master-secret-value",
		"PG_SAGE_GCP_ACCESS_TOKEN":           "ya29.token-value",
	}
	lookup := func(name string) (string, bool) { v, ok := set[name]; return v, ok }
	environ := []string{
		"PG_SAGE_AGENTDB_DEP_ENV=postgres://agent:pw@h/db",
		"PATH=/usr/bin",
		"PG_SAGE_AGENTDB_EMPTY=",
	}
	creds := credentials(lookup, environ, []string{"env:PG_SAGE_AGENTDB_DEP_ENV",
		"env:PG_SAGE_AGENTDB_GONE", "arn:aws:secretsmanager:x"})
	byName := map[string]Credential{}
	for _, c := range creds {
		if _, dup := byName[c.Name]; dup {
			t.Fatalf("credential %q listed twice", c.Name)
		}
		byName[c.Name] = c
		if strings.Contains(c.Name+c.Action, "secret-value") ||
			strings.Contains(c.Name+c.Action, "token-value") ||
			strings.Contains(c.Name+c.Action, "agent:pw") {
			t.Fatalf("credential entry carries a value: %+v", c)
		}
	}
	for _, name := range []string{"PG_SAGE_NEON_API_KEY", "PG_SAGE_SUPABASE_ACCESS_TOKEN",
		"PG_SAGE_SUPABASE_DATABASE_PASSWORD", "PG_SAGE_DATABRICKS_TOKEN",
		"PG_SAGE_GCP_ACCESS_TOKEN", "PG_SAGE_LIVE_PROVISIONING",
		"PG_SAGE_AGENTDB_LOCAL_PROVISIONING", "PG_SAGE_AGENTDB_DEP_ENV",
		"PG_SAGE_AGENTDB_GONE", "PG_SAGE_AGENTDB_EMPTY"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("credential %s not listed", name)
		}
	}
	for name, want := range map[string]bool{
		"PG_SAGE_SUPABASE_DATABASE_PASSWORD": true, "PG_SAGE_GCP_ACCESS_TOKEN": true,
		"PG_SAGE_AGENTDB_DEP_ENV": true, "PG_SAGE_NEON_API_KEY": false,
		"PG_SAGE_AGENTDB_GONE": false, "PG_SAGE_AGENTDB_EMPTY": false,
	} {
		if byName[name].Set != want {
			t.Errorf("%s set = %v, want %v", name, byName[name].Set, want)
		}
	}
	if byName["PG_SAGE_SUPABASE_DATABASE_PASSWORD"].Kind != "env" {
		t.Fatalf("kind = %q", byName["PG_SAGE_SUPABASE_DATABASE_PASSWORD"].Kind)
	}
	iam := 0
	for _, c := range creds {
		if c.Kind == "iam" {
			iam++
			if c.Provider == "" || c.Action == "" {
				t.Fatalf("iam grant without provider or action: %+v", c)
			}
		}
	}
	if iam < 3 {
		t.Fatalf("%d IAM grant entries, want the AWS, GCP and Databricks runbook grants", iam)
	}
	if _, ok := byName["arn:aws:secretsmanager:x"]; ok {
		t.Fatal("a non-env secret reference became an env credential")
	}
}

func TestSetCredentialNames_OnlySetEnvNamesSorted(t *testing.T) {
	creds := []Credential{
		{Kind: "env", Name: "B", Set: true}, {Kind: "env", Name: "A", Set: true},
		{Kind: "env", Name: "C"}, {Kind: "iam", Name: "rds:DeleteDBInstance", Set: true},
	}
	got := setCredentialNames(creds)
	if strings.Join(got, ",") != "A,B" {
		t.Fatalf("set names = %v, want [A B]", got)
	}
	if got := setCredentialNames(nil); len(got) != 0 {
		t.Fatalf("nil credentials gave %v", got)
	}
}
