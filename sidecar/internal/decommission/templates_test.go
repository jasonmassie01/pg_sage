package decommission

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

var updateTemplateGolden = flag.Bool("update-decommission-golden", false,
	"rewrite testdata/delete_templates.golden from the current templates")

const templateGoldenPath = "testdata/delete_templates.golden"

// templateCases is one item per provider arm and mode, plus the hostile
// values an operator must be able to paste safely.
func templateCases() []Item {
	return []Item{
		{ID: "rds", Kind: KindProviderResource, Provider: "aws_rds",
			ResourceID: "pgsage-dep-1", Region: "us-east-2", Account: "123456789012"},
		{ID: "rds-unknown-region", Kind: KindProviderResource, Provider: "aws_rds",
			ResourceID: "pgsage-dep-2", Region: unknown, Account: unknown},
		{ID: "rds-snapshot", Kind: KindRDSFinalSnapshot, Provider: "aws_rds",
			DeterministicName: "pgsage-dep-1-final-", Region: "us-east-2"},
		{ID: "rds-snapshot-odd", Kind: KindRDSFinalSnapshot, Provider: "aws_rds",
			DeterministicName: "odd'name-final-", Region: "us-east-2"},
		{ID: "cloudsql", Kind: KindProviderResource, Provider: "gcp_cloudsql",
			ResourceID: "pgsage-dep-3", Region: "us-central1", Account: "proj-a"},
		{ID: "neon-branch", Kind: KindProviderResource, Provider: "neon", Mode: "branch",
			ResourceID: "br-cool-42", Account: "proj-9"},
		{ID: "neon-project", Kind: KindProviderResource, Provider: "neon", Mode: "project",
			ResourceID: "proj-77", Account: "org-7"},
		{ID: "supabase-branch", Kind: KindProviderResource, Provider: "supabase",
			Mode: "branch", ResourceID: "br-uuid-1", Account: "abcdefghijklmnop"},
		{ID: "supabase-project", Kind: KindProviderResource, Provider: "supabase",
			Mode: "project", ResourceID: "qrstuvwxyzabcdef", Account: "org-1"},
		{ID: "lakebase", Kind: KindProviderResource, Provider: "databricks_lakebase",
			ResourceID: "pgsage-dep-4", Account: "lake/proj"},
		{ID: "uncertain", Kind: KindProviderResource, Provider: "neon", Mode: "branch",
			DeterministicName: "pgsage-dep-5-0a1b2c3d", Account: "proj-9"},
		{ID: "unknown-provider", Kind: KindProviderResource, Provider: "heroku",
			ResourceID: "h-1"},
		{ID: "local-schema", Kind: KindLocalSchema, Provider: "local_postgres",
			ResourceID: `agent"db_s`, Account: "sage_meta"},
		{ID: "local-database", Kind: KindLocalDatabase, Provider: "local_postgres",
			ResourceID: "agentdb_db", Account: "sage_meta"},
		{ID: "sage-schema-env", Kind: KindAgentSageSchema, Provider: "neon",
			ResourceID: "ep-1.neon.tech/app", EnvRef: "env:PG_SAGE_AGENTDB_X"},
		{ID: "sage-schema-inline", Kind: KindAgentSageSchema, Provider: "local_postgres",
			ResourceID: "db.internal:6543/agent"},
		{ID: "hostile", Kind: KindProviderResource, Provider: "gcp_cloudsql",
			ResourceID: "x'; rm -rf / #\nDROP", Region: "r$(id)", Account: "p`id`"},
	}
}

func renderTemplates(items []Item) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "== %s\n", it.ID)
		for _, line := range deleteTemplate(it) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// G0-01: the per-provider delete templates are pinned.
func TestDeleteTemplates_MatchGolden(t *testing.T) {
	got := renderTemplates(templateCases())
	if *updateTemplateGolden {
		if err := os.WriteFile(templateGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(templateGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update-decommission-golden once): %v", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Fatalf("templates drifted from %s:\n%s", templateGoldenPath, got)
	}
}

func TestDeleteTemplates_EveryLineIsOneLineAndHostileValuesStayQuoted(t *testing.T) {
	for _, it := range templateCases() {
		lines := deleteTemplate(it)
		if len(lines) == 0 {
			t.Fatalf("%s: empty template", it.ID)
		}
		for _, line := range lines {
			if strings.ContainsAny(line, "\n\r\x00") {
				t.Fatalf("%s: a template line carries a control character: %q", it.ID, line)
			}
		}
	}
	hostile := strings.Join(deleteTemplate(templateCases()[len(templateCases())-1]), "\n")
	for _, bad := range []string{"--project=p`id`", "r$(id) ", "x'; rm"} {
		if strings.Contains(hostile, bad) {
			t.Fatalf("hostile value %q reached the template unquoted:\n%s", bad, hostile)
		}
	}
}

func TestDeleteTemplates_ProviderSpecificSafetySteps(t *testing.T) {
	byID := map[string]string{}
	for _, it := range templateCases() {
		byID[it.ID] = strings.Join(deleteTemplate(it), "\n")
	}
	checks := []struct{ id, want string }{
		// Cloud SQL lifts deletion protection before the delete.
		{"cloudsql", "--no-deletion-protection"},
		// RDS makes the operator choose the final snapshot.
		{"rds", "--final-db-snapshot-identifier <choose"},
		{"rds", "--skip-final-snapshot"},
		// Neon has separate project and branch commands.
		{"neon-branch", "neonctl branches delete 'br-cool-42' --project-id 'proj-9'"},
		{"neon-project", "neonctl projects delete 'proj-77'"},
		{"uncertain", "pgsage-dep-5-0a1b2c3d"},
		{"unknown-provider", "no delete template"},
	}
	for _, c := range checks {
		if !strings.Contains(byID[c.id], c.want) {
			t.Errorf("%s template lacks %q:\n%s", c.id, c.want, byID[c.id])
		}
	}
	if strings.Index(byID["cloudsql"], "patch") > strings.Index(byID["cloudsql"], "delete") {
		t.Fatal("Cloud SQL must lift deletion protection before deleting")
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":       "''",
		"plain":  "'plain'",
		"it's":   `'it'\''s'`,
		"$(x) `": "'$(x) `'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
