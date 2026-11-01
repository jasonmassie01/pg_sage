package cases

import (
	"strings"
	"testing"
)

func TestLakebaseServerSettingAdviceIsWithheldRatherThanExported(t *testing.T) {
	for _, sql := range []string{
		"ALTER SYSTEM SET wal_compression = on;",
		"ALTER\nSYSTEM SET wal_compression = on;",
		"ALTER /* server setting */ SYSTEM RESET wal_compression;",
	} {
		c := ProjectFinding(SourceFinding{Category: "migration_safety", Recommendation: "Enable WAL compression.", RecommendedSQL: sql})
		ApplyProviderApplicability(&c, "lakebase")
		a := c.ActionCandidates[0]
		if a.ProposedSQL != "" || a.ScriptOutput != nil || len(a.OutputModes) != 0 || a.RollbackClass != "not_applicable" {
			t.Fatalf("Lakebase must not export inapplicable setting procedure: %#v", a)
		}
		if !strings.Contains(c.Why, "Lakebase manages instance settings") || a.BlockedReason == "" {
			t.Fatalf("applicability must be explained: %#v", c)
		}
	}
}

func TestProviderApplicabilityKeepsGenericPostgresAndScopedSettings(t *testing.T) {
	for _, test := range []struct{ provider, sql string }{
		{"postgres", "ALTER SYSTEM SET wal_compression = on;"},
		{"", "ALTER SYSTEM SET wal_compression = on;"},
		{"lakebase", "ALTER DATABASE application SET work_mem = '32MB';"},
		{"lakebase", "ALTER ROLE reporter SET work_mem = '32MB';"},
	} {
		c := ProjectFinding(SourceFinding{RecommendedSQL: test.sql, Recommendation: "Review this proposal."})
		ApplyProviderApplicability(&c, test.provider)
		if c.ActionCandidates[0].ProposedSQL != test.sql || c.Why != "Review this proposal." {
			t.Fatalf("provider %q scoped SQL changed: %#v", test.provider, c)
		}
	}
}

func TestProviderApplicabilityUsesPolicyIdentityAndChecksScriptOnlyAdvice(t *testing.T) {
	for _, provider := range []string{"", "unknown", " UNKNOWN "} {
		c := Case{ActionCandidates: []ActionCandidate{{
			ActionType: "ddl_preflight", OutputModes: []string{"generate_pr_or_script"},
			ScriptOutput:   &ScriptOutput{MigrationSQL: "ALTER SYSTEM SET wal_compression = on;"},
			PolicyDecision: &ActionPolicyDecision{Provider: "lakebase", Decision: "approval_required", RequiresApproval: true},
		}}}
		ApplyProviderApplicability(&c, provider)
		a := c.ActionCandidates[0]
		if a.ScriptOutput != nil || a.PolicyDecision.Decision != "blocked" || a.PolicyDecision.BlockedReason != "provider_managed_setting" || a.PolicyDecision.RequiresApproval {
			t.Fatalf("policy-derived applicability not applied: %#v", a)
		}
	}
}
