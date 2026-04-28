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

func TestProjectFindingTableBloatAddsVacuumAutopilotCandidate(t *testing.T) {
	f := SourceFinding{
		ID:               "bloat-1",
		DatabaseName:     "prod",
		Category:         "table_bloat",
		Severity:         SeverityWarning,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "orders has high dead tuple ratio",
		Recommendation:   "Run VACUUM on public.orders",
		RecommendedSQL:   "VACUUM public.orders;",
		Detail: map[string]any{
			"dead_ratio":   0.42,
			"n_dead_tup":   float64(420000),
			"io_saturated": false,
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "vacuum_table", "safe", f.RecommendedSQL)
	if action.RollbackClass != "no_rollback_needed" {
		t.Fatalf("RollbackClass = %q", action.RollbackClass)
	}
	if len(action.VerificationPlan) < 2 {
		t.Fatalf("VerificationPlan too short: %#v", action.VerificationPlan)
	}
}

func TestProjectFindingBloatIOSaturatedBlocksAutonomousVacuum(t *testing.T) {
	f := SourceFinding{
		ID:               "bloat-io",
		DatabaseName:     "prod",
		Category:         "table_bloat",
		Severity:         SeverityCritical,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "orders bloat is high while IO is saturated",
		Recommendation:   "Defer VACUUM until IO pressure clears",
		RecommendedSQL:   "VACUUM public.orders;",
		Detail: map[string]any{
			"dead_ratio":    0.55,
			"n_dead_tup":    float64(900000),
			"io_saturated":  true,
			"io_wait_ratio": 0.42,
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	if action.BlockedReason != "IO is saturated; wait for maintenance window or lower load" {
		t.Fatalf("BlockedReason = %q", action.BlockedReason)
	}
	if len(action.OutputModes) != 1 || action.OutputModes[0] != "generate_pr_or_script" {
		t.Fatalf("OutputModes = %#v, want script-only", action.OutputModes)
	}
}

func TestProjectFindingXIDWraparoundAddsFreezeDiagnosticCandidate(t *testing.T) {
	f := SourceFinding{
		ID:               "xid-1",
		DatabaseName:     "prod",
		Category:         "xid_wraparound",
		Severity:         SeverityCritical,
		ObjectType:       "database",
		ObjectIdentifier: "prod",
		Title:            "XID runway is low",
		Recommendation:   "Find freeze blockers and oldest xmin holders.",
		Detail: map[string]any{
			"age_datfrozenxid": float64(1800000000),
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_freeze_blockers", "safe", "")
	if got.ActionCandidates[0].ProposedSQL == "" {
		t.Fatal("expected freeze diagnostic SQL")
	}
}

func TestProjectFindingVacuumTuningAddsAutovacuumScript(t *testing.T) {
	f := SourceFinding{
		ID:               "tune-1",
		DatabaseName:     "prod",
		Category:         "vacuum_tuning",
		Severity:         SeverityWarning,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "orders needs tighter autovacuum settings",
		Recommendation:   "Lower scale factor for high-churn table.",
		RecommendedSQL: "ALTER TABLE public.orders SET " +
			"(autovacuum_vacuum_scale_factor = 0.02);",
		Detail: map[string]any{
			"current_scale_factor":     0.2,
			"recommended_scale_factor": 0.02,
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "set_table_autovacuum", "moderate", f.RecommendedSQL)
	if action.ScriptOutput == nil {
		t.Fatal("expected PR/CI script output for autovacuum tuning")
	}
	if action.RequiresMaintenanceWindow {
		t.Fatal("autovacuum reloption tuning should not require maintenance window")
	}
}

func TestProjectFindingBloatRemediationAddsPlanCandidate(t *testing.T) {
	f := SourceFinding{
		ID:               "bloat-plan",
		DatabaseName:     "prod",
		Category:         "bloat_remediation",
		Severity:         SeverityCritical,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "orders needs online bloat remediation",
		Recommendation:   "Plan pg_repack or online rebuild.",
		Detail: map[string]any{
			"bloat_ratio": 0.68,
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "plan_bloat_remediation", "high", "")
	if action.ScriptOutput == nil {
		t.Fatal("expected bloat remediation script output")
	}
}

func TestProjectFindingReindexCandidateUsesConcurrentReindex(t *testing.T) {
	f := SourceFinding{
		ID:               "reindex-1",
		DatabaseName:     "prod",
		Category:         "reindex_candidate",
		Severity:         SeverityWarning,
		ObjectType:       "index",
		ObjectIdentifier: "public.idx_orders_status",
		Title:            "index bloat is high",
		Recommendation:   "Reindex concurrently during maintenance.",
		RecommendedSQL:   "REINDEX INDEX CONCURRENTLY public.idx_orders_status;",
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"reindex_concurrently", "moderate", f.RecommendedSQL)
}

func TestProjectFindingBlockedVacuumAddsDiagnosticCandidate(t *testing.T) {
	f := SourceFinding{
		ID:               "blocked-vacuum",
		DatabaseName:     "prod",
		Category:         "blocked_vacuum",
		Severity:         SeverityCritical,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "vacuum is blocked on orders",
		Recommendation:   "Find blockers before running maintenance.",
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	assertCandidate(t, got.ActionCandidates[0],
		"diagnose_vacuum_pressure", "safe", "")
	if got.ActionCandidates[0].ProposedSQL == "" {
		t.Fatal("expected vacuum pressure diagnostic SQL")
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

func TestProjectQueryHintWithRewriteAddsPRAction(t *testing.T) {
	before, after := 2500.0, 900.0
	hint := SourceQueryHint{
		QueryID:          444,
		DatabaseName:     "prod",
		HintText:         "HashJoin(o c)",
		Symptom:          "nested loop row estimate skew",
		Status:           "active",
		CreatedAt:        time.Now().UTC(),
		BeforeCost:       &before,
		AfterCost:        &after,
		SuggestedRewrite: "SELECT * FROM orders o JOIN customers c ON c.id = o.customer_id",
		RewriteRationale: "make join predicate explicit for planner stability",
	}

	got := ProjectQueryHint(hint)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "prepare_query_rewrite", "moderate", "")
	if action.ScriptOutput == nil {
		t.Fatal("expected query rewrite script output")
	}
	if action.ScriptOutput.MigrationSQL == "" {
		t.Fatal("expected rewrite SQL in script output")
	}
	if action.RollbackClass != "application_rollback" {
		t.Fatalf("RollbackClass = %q", action.RollbackClass)
	}
}

func TestProjectBrokenQueryHintAddsRetireAction(t *testing.T) {
	hint := SourceQueryHint{
		QueryID:      555,
		DatabaseName: "prod",
		HintText:     "Set(work_mem \"512MB\")",
		Symptom:      "hint regressed during revalidation",
		Status:       "broken",
		CreatedAt:    time.Now().UTC(),
	}

	got := ProjectQueryHint(hint)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "retire_query_hint", "safe", "")
	if action.VerificationPlan[0] != "verify hint no longer appears in active hints" {
		t.Fatalf("VerificationPlan = %#v", action.VerificationPlan)
	}
}

func TestProjectFindingRoleWorkMemPromotionAddsReviewedAction(t *testing.T) {
	f := SourceFinding{
		ID:               "wm-role",
		DatabaseName:     "prod",
		Category:         "query_work_mem_promotion",
		Severity:         SeverityWarning,
		ObjectType:       "role",
		ObjectIdentifier: "app_user",
		Title:            "app_user has repeated work_mem hints",
		Recommendation:   "Promote repeated per-query hints to a role setting.",
		RecommendedSQL:   "ALTER ROLE app_user SET work_mem = '128MB';",
		Detail: map[string]any{
			"role_name":       "app_user",
			"hint_count":      float64(8),
			"recommended_mb":  float64(128),
			"sample_query_id": float64(42),
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "promote_role_work_mem", "moderate", f.RecommendedSQL)
	if action.ScriptOutput == nil {
		t.Fatal("expected role setting script output")
	}
	if action.RequiresMaintenanceWindow {
		t.Fatal("role work_mem promotion should not require maintenance window")
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

func TestProjectFindingMigrationSafetyIncludesLivePreflightChecks(t *testing.T) {
	f := SourceFinding{
		ID:               "ddl-live",
		DatabaseName:     "prod",
		Category:         "migration_safety",
		Severity:         SeverityCritical,
		ObjectType:       "migration",
		ObjectIdentifier: "public.orders:ddl_alter_type_rewrite:abc",
		Title:            "Dangerous DDL: rewrite",
		Recommendation:   "Review safer migration path.",
		RecommendedSQL:   "ALTER TABLE public.orders ALTER id TYPE bigint",
		Detail: map[string]any{
			"rule_id":          "ddl_alter_type_rewrite",
			"requires_rewrite": true,
			"affected_table":   "public.orders",
			"table_size_bytes": float64(14 * 1024 * 1024 * 1024),
			"estimated_rows":   float64(90000000),
			"active_queries":   float64(4),
			"pending_locks":    float64(2),
			"replication_lag":  75.0,
			"lock_timeout_ms":  float64(0),
		},
	}

	got := ProjectFinding(f)

	action := got.ActionCandidates[0]
	if action.DDLPreflight == nil {
		t.Fatal("expected DDL preflight")
	}
	assertPreflightCheck(t, action.DDLPreflight.Checks,
		"table_size", "warn")
	assertPreflightCheck(t, action.DDLPreflight.Checks,
		"lock_timeout", "warn")
	if action.BlockedReason == "" {
		t.Fatal("expected live risk to block direct execution")
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

func TestProjectFindingCreateStatisticsAddsReviewedAction(t *testing.T) {
	f := SourceFinding{
		ID:               "stats-1",
		DatabaseName:     "prod",
		Category:         "query_create_statistics",
		Severity:         SeverityWarning,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "Correlated predicates need extended stats",
		Recommendation:   "Create dependency statistics for planner estimates.",
		RecommendedSQL: "CREATE STATISTICS stats_orders_customer_status " +
			"(dependencies) ON customer_id, status FROM public.orders;",
		Detail: map[string]any{
			"sample_query": "select * from orders where customer_id=$1 and status=$2",
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "create_statistics", "moderate", f.RecommendedSQL)
	if action.ScriptOutput == nil {
		t.Fatal("expected CREATE STATISTICS script output")
	}
}

func TestProjectFindingParameterizedQueryAddsExperimentAction(t *testing.T) {
	f := SourceFinding{
		ID:               "param-1",
		DatabaseName:     "prod",
		Category:         "query_parameterization",
		Severity:         SeverityWarning,
		ObjectType:       "query",
		ObjectIdentifier: "queryid:88",
		Title:            "Literal-heavy query should be parameterized",
		Recommendation:   "Parameterize literal query shapes in application code.",
		Detail: map[string]any{
			"normalized_query": "select * from orders where id = ?",
			"literal_count":    float64(1200),
		},
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 1 {
		t.Fatalf("ActionCandidates = %d, want 1", len(got.ActionCandidates))
	}
	action := got.ActionCandidates[0]
	assertCandidate(t, action, "prepare_parameterized_query", "moderate", "")
	if action.ScriptOutput == nil || action.ScriptOutput.PRBody == "" {
		t.Fatal("expected parameterization PR output")
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

func assertPreflightCheck(
	t *testing.T,
	checks []PreflightCheck,
	name string,
	status string,
) {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			if check.Status != status {
				t.Fatalf("%s status = %q, want %q",
					name, check.Status, status)
			}
			return
		}
	}
	t.Fatalf("missing preflight check %q in %#v", name, checks)
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
