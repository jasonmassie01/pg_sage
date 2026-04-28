package cases

import (
	"testing"
	"time"
)

func TestIdentityKeyFindingUsesStableProblemFields(t *testing.T) {
	f := SourceFinding{
		DatabaseName:     "prod",
		Category:         "schema_lint:lint_no_primary_key",
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		RuleID:           "lint_no_primary_key",
	}

	got := IdentityKeyForFinding(f)
	want := "finding:prod:schema_lint:lint_no_primary_key:table:public.orders:lint_no_primary_key"
	if got != want {
		t.Fatalf("identity key = %q, want %q", got, want)
	}
}

func TestIdentityKeyQueryPrefersNormalizedFingerprint(t *testing.T) {
	f := SourceFinding{
		DatabaseName:     "prod",
		Category:         "query_tuning",
		ObjectType:       "query",
		ObjectIdentifier: "query_id:123",
		Detail: map[string]any{
			"normalized_query": "select * from orders where id = ?",
		},
	}

	got := IdentityKeyForFinding(f)
	want := "finding:prod:query_tuning:query:select * from orders where id = ?"
	if got != want {
		t.Fatalf("identity key = %q, want %q", got, want)
	}
}

func TestNewCaseRequiresWhyNowEvenWhenNotUrgent(t *testing.T) {
	c := NewCase(CaseInput{
		SourceType:   SourceFindingType,
		SourceID:     "42",
		DatabaseName: "prod",
		IdentityKey:  "finding:prod:test",
		Title:        "Test case",
		Severity:     SeverityInfo,
		Evidence: []Evidence{{
			Type:    "finding",
			Summary: "test evidence",
		}},
	})

	if c.WhyNow != "not urgent" {
		t.Fatalf("WhyNow = %q, want not urgent", c.WhyNow)
	}
	if c.State != StateOpen {
		t.Fatalf("State = %q, want %q", c.State, StateOpen)
	}
}

func TestProjectFindingCreatesActionableCase(t *testing.T) {
	f := SourceFinding{
		ID:               "42",
		DatabaseName:     "prod",
		Category:         "stale_stats",
		Severity:         SeverityWarning,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "Stats are stale",
		Recommendation:   "Run ANALYZE on public.orders",
		RecommendedSQL:   "ANALYZE public.orders",
		Detail: map[string]any{
			"n_mod_since_analyze": float64(200000),
			"last_analyze_age":    "72h",
		},
	}

	got := ProjectFinding(f)

	if got.SourceType != SourceFindingType {
		t.Fatalf("SourceType = %q", got.SourceType)
	}
	if got.ActionCandidates[0].ActionType != "analyze_table" {
		t.Fatalf("ActionType = %q", got.ActionCandidates[0].ActionType)
	}
	if got.ActionCandidates[0].RiskTier != "safe" {
		t.Fatalf("RiskTier = %q", got.ActionCandidates[0].RiskTier)
	}
	if got.WhyNow == "not urgent" {
		t.Fatalf("WhyNow was not populated from stale-stat detail")
	}
}

func TestProjectFindingClassifiesForecastAsForecastCase(t *testing.T) {
	f := SourceFinding{
		ID:               "77",
		DatabaseName:     "prod",
		Category:         "forecast_connection_exhaustion",
		Severity:         SeverityCritical,
		ObjectType:       "database",
		ObjectIdentifier: "prod",
		Title:            "Connection pool exhaustion forecast",
		Recommendation:   "Review pool limits before saturation.",
		Detail: map[string]any{
			"projected_at": "2026-04-29T12:00:00Z",
		},
	}

	got := ProjectFinding(f)

	if got.SourceType != SourceForecastType {
		t.Fatalf("SourceType = %q, want %q",
			got.SourceType, SourceForecastType)
	}
	if got.WhyNow == "not urgent" {
		t.Fatalf("expected forecast urgency detail")
	}
}

func TestProjectFindingClassifiesSchemaLintAsSchemaCase(t *testing.T) {
	f := SourceFinding{
		ID:               "88",
		DatabaseName:     "prod",
		Category:         "schema_lint:lint_no_primary_key",
		Severity:         SeverityWarning,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		RuleID:           "lint_no_primary_key",
		Title:            "Table has no primary key",
		Recommendation:   "Add a primary key during the next migration.",
	}

	got := ProjectFinding(f)

	if got.SourceType != SourceSchemaType {
		t.Fatalf("SourceType = %q, want %q",
			got.SourceType, SourceSchemaType)
	}
	if got.Evidence[0].Type != "schema_health" {
		t.Fatalf("Evidence type = %q, want schema_health",
			got.Evidence[0].Type)
	}
}

func TestProjectQueryHintCreatesCaseWithExperimentEvidence(t *testing.T) {
	createdAt := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	before, after := 100.0, 42.0
	hint := SourceQueryHint{
		QueryID:          12345,
		DatabaseName:     "prod",
		HintText:         "IndexScan(users idx_users_email)",
		Symptom:          "planner chose a sequential scan",
		Status:           "active",
		CreatedAt:        createdAt,
		BeforeCost:       &before,
		AfterCost:        &after,
		SuggestedRewrite: "select * from users where email = $1",
		RewriteRationale: "preserve index predicate shape",
	}

	got := ProjectQueryHint(hint)

	if got.SourceType != SourceQueryType {
		t.Fatalf("SourceType = %q, want %q",
			got.SourceType, SourceQueryType)
	}
	if got.ID == "" || got.IdentityKey == "" {
		t.Fatalf("expected stable id and identity key: %#v", got)
	}
	if got.Title != "Query hint active for query 12345" {
		t.Fatalf("Title = %q", got.Title)
	}
	if len(got.Evidence) != 1 {
		t.Fatalf("evidence len = %d, want 1", len(got.Evidence))
	}
	if got.Evidence[0].Detail["before_cost"] != before {
		t.Fatalf("before_cost = %#v, want %v",
			got.Evidence[0].Detail["before_cost"], before)
	}
	if got.Evidence[0].Detail["suggested_rewrite"] == "" {
		t.Fatalf("expected suggested rewrite evidence")
	}
}

func TestProjectFindingInformationalWhenNoRemediation(t *testing.T) {
	f := SourceFinding{
		ID:               "99",
		DatabaseName:     "prod",
		Category:         "schema_lint:lint_serial_usage",
		Severity:         SeverityInfo,
		ObjectType:       "column",
		ObjectIdentifier: "public.orders.id",
		Title:            "Legacy serial usage",
		Recommendation:   "Prefer identity columns for new schema.",
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected no action candidates, got %d", len(got.ActionCandidates))
	}
	if got.State != StateOpen {
		t.Fatalf("State = %q", got.State)
	}
}

func TestProjectFindingMarksAlterTableForwardFixOnly(t *testing.T) {
	f := SourceFinding{
		ID:               "101",
		DatabaseName:     "prod",
		Category:         "schema_change",
		Severity:         SeverityCritical,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "Column type needs review",
		Recommendation:   "Review widening the column type.",
		RecommendedSQL:   "ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric",
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("action candidates = %d, want 1",
			len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	if action.ActionType != "alter_table" {
		t.Fatalf("ActionType = %q, want alter_table", action.ActionType)
	}
	if action.RiskTier != "high" {
		t.Fatalf("RiskTier = %q, want high", action.RiskTier)
	}
	if action.RollbackClass != "forward_fix_only" {
		t.Fatalf("RollbackClass = %q, want forward_fix_only",
			action.RollbackClass)
	}
}

func TestProjectFindingMigrationSafetyCreateIndexCandidate(t *testing.T) {
	f := SourceFinding{
		ID:               "202",
		DatabaseName:     "prod",
		Category:         "migration_safety",
		Severity:         SeverityWarning,
		ObjectType:       "migration",
		ObjectIdentifier: "public.orders:ddl_index_not_concurrent:abc",
		Title:            "Dangerous DDL: ddl_index_not_concurrent",
		Recommendation:   "Review the safer migration path.",
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY idx_orders_id ON orders (id)",
		Detail: map[string]any{
			"rule_id":        "ddl_index_not_concurrent",
			"risk_score":     0.7,
			"original_sql":   "CREATE INDEX idx_orders_id ON orders (id)",
			"database_name":  "prod",
			"action_risk":    "risk_score=0.70",
			"affected_table": "public.orders",
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("action candidates = %d, want 1",
			len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	if action.ActionType != "create_index_concurrently" {
		t.Fatalf("ActionType = %q, want create_index_concurrently",
			action.ActionType)
	}
	if action.RiskTier != "moderate" {
		t.Fatalf("RiskTier = %q, want moderate", action.RiskTier)
	}
	if action.RollbackClass != "reversible" {
		t.Fatalf("RollbackClass = %q, want reversible",
			action.RollbackClass)
	}
	if action.DDLPreflight == nil {
		t.Fatal("expected DDL preflight report")
	}
	if action.DDLPreflight.LockLevel == "" {
		t.Fatalf("expected lock-level preflight detail")
	}
	if action.ScriptOutput == nil {
		t.Fatal("expected PR/CI script output")
	}
	if action.ScriptOutput.MigrationSQL != f.RecommendedSQL {
		t.Fatalf("MigrationSQL = %q, want %q",
			action.ScriptOutput.MigrationSQL, f.RecommendedSQL)
	}
	if action.ScriptOutput.RollbackSQL == "" {
		t.Fatal("expected rollback SQL for reversible index change")
	}
	if len(action.ScriptOutput.VerificationSQL) == 0 {
		t.Fatal("expected verification SQL")
	}
	if action.ScriptOutput.PRTitle == "" || action.ScriptOutput.PRBody == "" {
		t.Fatalf("expected PR title/body: %#v", action.ScriptOutput)
	}
}

func TestProjectFindingMigrationSafetyAdvisoryOnlyProducesPreflightScript(t *testing.T) {
	f := SourceFinding{
		ID:               "203",
		DatabaseName:     "prod",
		Category:         "migration_safety",
		Severity:         SeverityCritical,
		ObjectType:       "migration",
		ObjectIdentifier: "public.orders:ddl_alter_type_rewrite:abc",
		Title:            "Dangerous DDL: ddl_alter_type_rewrite",
		Recommendation:   "Review a safe rollout plan.",
		RecommendedSQL:   "",
		Detail: map[string]any{
			"safe_alternative": "New column + trigger + backfill + swap",
			"original_sql":     "ALTER TABLE orders ALTER id TYPE bigint",
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("expected one action candidate, got %d",
			len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	if action.ActionType != "ddl_preflight" {
		t.Fatalf("ActionType = %q, want ddl_preflight",
			action.ActionType)
	}
	if action.RiskTier != "high" {
		t.Fatalf("RiskTier = %q, want high", action.RiskTier)
	}
	if action.RollbackClass != "forward_fix_only" {
		t.Fatalf("RollbackClass = %q, want forward_fix_only",
			action.RollbackClass)
	}
	if action.ProposedSQL != "" {
		t.Fatalf("ProposedSQL = %q, want non-executing candidate",
			action.ProposedSQL)
	}
	if action.ScriptOutput == nil {
		t.Fatal("expected script output")
	}
	if action.ScriptOutput.MigrationSQL == "" {
		t.Fatal("expected generated forward-fix migration script")
	}
	if action.ScriptOutput.RollbackSQL != "" {
		t.Fatalf("RollbackSQL = %q, want empty for forward-fix only",
			action.ScriptOutput.RollbackSQL)
	}
	if got.Evidence[0].Detail["safe_alternative"] == "" {
		t.Fatal("expected advisory safe alternative to stay in evidence")
	}
}

func TestResolveEphemeralWhenEvidenceDisappears(t *testing.T) {
	open := NewCase(CaseInput{
		SourceType:   SourceFindingType,
		SourceID:     "1",
		DatabaseName: "prod",
		IdentityKey:  "finding:prod:lock",
		Title:        "Lock pileup",
		Severity:     SeverityWarning,
		Evidence:     []Evidence{{Type: "lock", Summary: "blocked sessions"}},
		ActionCandidates: []ActionCandidate{{
			ActionType: "cancel_backend",
			RiskTier:   "moderate",
		}},
	})

	got := ResolveIfEvidenceMissing(open, false)

	if got.State != StateResolvedEphemeral {
		t.Fatalf("State = %q, want %q", got.State, StateResolvedEphemeral)
	}
	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected pending candidates to clear")
	}
}

func TestExpiredActionCannotExecuteWithoutRevalidation(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	c := ActionCandidate{
		ActionType: "analyze_table",
		RiskTier:   "safe",
		ExpiresAt:  &expired,
	}

	if c.IsExecutable(time.Now()) {
		t.Fatalf("expired action should not be executable")
	}
}
