package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

// lifeosStoredPolicy is lifeos's standing policy as stored since
// 2026-09-05 (sage.policy id 1): the legacy single blast-radius limit.
const lifeosStoredPolicy = `{"budgets": {"spend_daily": null, "storage_bytes": null,
 "llm_tokens_daily": 500000},
 "rate_limits": {"max_self_initiated_changes_per_window": 50},
 "refusal_set": ["rls_change", "grant_expansion", "major_upgrade", "non_dup_object_drop",
  "unrollbackable"],
 "blast_radius": {"max_rows_rewritten": 5000000, "max_tables_per_window": 20},
 "serialize_mode": "park", "deadline_overrides": {"xid": true, "disk": true},
 "maintenance_windows": ["always", "weekends"],
 "allowed_change_classes": ["index", "analyze", "vacuum", "freeze", "autovacuum_tuning",
  "config_guc", "retention", "fk_index", "online_migration", "backend_signal",
  "query_hint", "schema_change"],
 "unknown_classification": "fail_closed", "lock_duration_ceiling_ms": 3000,
 "approval_required_classes": ["online_migration"]}`

// policyWithBlastRadius is a valid document whose blast_radius and
// rate_limits members are replaced by the given raw JSON (empty omits).
func policyWithBlastRadius(blastRadius, rateLimits string) string {
	doc := `{"allowed_change_classes":["index","analyze","vacuum"],
	"maintenance_windows":["always"], "lock_duration_ceiling_ms":3000,
	"budgets":{"storage_bytes":null,"spend_daily":null,"llm_tokens_daily":500000},
	"deadline_overrides":{"xid":false,"disk":false}, "refusal_set":["rls_change"],
	"unknown_classification":"fail_closed", "serialize_mode":"park"`
	if blastRadius != "" {
		doc += `, "blast_radius":` + blastRadius
	}
	if rateLimits != "" {
		doc += `, "rate_limits":` + rateLimits
	}
	return doc + "}"
}

func mustParse(t *testing.T, raw string) Document {
	t.Helper()
	doc, err := ParseDocument([]byte(raw))
	if err != nil {
		t.Fatalf("ParseDocument: %v\n%s", err, raw)
	}
	return doc
}

func defaultHygiene() KindBudget {
	return KindBudget{
		MaxTablesPerWindow:  DefaultHygieneTablesPerWindow,
		MaxChangesPerWindow: DefaultHygieneChangesPerWindow,
	}
}

// A stored document from before the split keeps its single limit as the
// performance budget, and hygiene gets its own default budget (decision
// 2026-10-03), so hygiene can never again spend the performance budget.
func TestLegacyDocumentLimitsBecomePerformanceBudget(t *testing.T) {
	doc := mustParse(t, lifeosStoredPolicy)
	if got := doc.Budget(BudgetPerformance); got != (KindBudget{20, 50}) {
		t.Fatalf("performance budget = %+v, want the legacy 20 tables / 50 changes", got)
	}
	if got := doc.Budget(BudgetHygiene); got != defaultHygiene() {
		t.Fatalf("hygiene budget = %+v, want the default %+v", got, defaultHygiene())
	}
	if DefaultHygieneTablesPerWindow <= 0 || DefaultHygieneChangesPerWindow <= 0 {
		t.Fatalf("hygiene defaults %d/%d must not silently disable hygiene",
			DefaultHygieneTablesPerWindow, DefaultHygieneChangesPerWindow)
	}
	if doc.BlastRadius.MaxRowsRewritten != 5000000 {
		t.Fatalf("max_rows_rewritten = %d, want 5000000", doc.BlastRadius.MaxRowsRewritten)
	}
}

func TestKindBudgetsParse(t *testing.T) {
	doc := mustParse(t, policyWithBlastRadius(`{"max_rows_rewritten":10,
		"performance":{"max_tables_per_window":7,"max_changes_per_window":9},
		"hygiene":{"max_tables_per_window":3,"max_changes_per_window":4}}`, ""))
	if got := doc.Budget(BudgetPerformance); got != (KindBudget{7, 9}) {
		t.Fatalf("performance = %+v, want {7 9}", got)
	}
	if got := doc.Budget(BudgetHygiene); got != (KindBudget{3, 4}) {
		t.Fatalf("hygiene = %+v, want {3 4}", got)
	}
	if doc.BlastRadius.MaxRowsRewritten != 10 {
		t.Fatalf("max_rows_rewritten = %d, want 10", doc.BlastRadius.MaxRowsRewritten)
	}
}

// A partly written hygiene block takes the default for what it leaves out;
// an explicit zero is a real limit (that kind may change nothing).
func TestKindBudgetsMissingFieldsAndExplicitZero(t *testing.T) {
	tests := []struct {
		name    string
		hygiene string
		want    KindBudget
	}{
		{"tables only", `{"max_tables_per_window":2}`,
			KindBudget{2, DefaultHygieneChangesPerWindow}},
		{"changes only", `{"max_changes_per_window":6}`,
			KindBudget{DefaultHygieneTablesPerWindow, 6}},
		{"empty block", `{}`, defaultHygiene()},
		{"explicit zero", `{"max_tables_per_window":0,"max_changes_per_window":0}`,
			KindBudget{0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustParse(t, policyWithBlastRadius(`{"max_rows_rewritten":1,
				"max_tables_per_window":20,"hygiene":`+tt.hygiene+`}`,
				`{"max_self_initiated_changes_per_window":50}`))
			if got := doc.Budget(BudgetHygiene); got != tt.want {
				t.Fatalf("hygiene = %+v, want %+v", got, tt.want)
			}
			if got := doc.Budget(BudgetPerformance); got != (KindBudget{20, 50}) {
				t.Fatalf("performance = %+v, want {20 50}", got)
			}
		})
	}
}

// The legacy names and the performance block name the same limits: equal
// values are accepted, different ones are a conflict, never a silent pick.
func TestKindBudgetsLegacyAndPerformanceMustAgree(t *testing.T) {
	agree := policyWithBlastRadius(`{"max_rows_rewritten":1,"max_tables_per_window":20,
		"performance":{"max_tables_per_window":20,"max_changes_per_window":50}}`,
		`{"max_self_initiated_changes_per_window":50}`)
	if got := mustParse(t, agree).Budget(BudgetPerformance); got != (KindBudget{20, 50}) {
		t.Fatalf("agreeing limits: performance = %+v, want {20 50}", got)
	}
	conflicts := map[string]string{
		"tables": policyWithBlastRadius(`{"max_rows_rewritten":1,"max_tables_per_window":20,
			"performance":{"max_tables_per_window":5,"max_changes_per_window":50}}`, ""),
		"changes": policyWithBlastRadius(`{"max_rows_rewritten":1,
			"performance":{"max_tables_per_window":5,"max_changes_per_window":7}}`,
			`{"max_self_initiated_changes_per_window":50}`),
	}
	for name, raw := range conflicts {
		_, err := ParseDocument([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "conflict") {
			t.Errorf("%s: err = %v, want a conflict error", name, err)
		}
	}
}

func TestKindBudgetsRejectInvalidValues(t *testing.T) {
	tests := map[string]string{
		"negative performance tables": `{"max_rows_rewritten":1,
			"performance":{"max_tables_per_window":-1,"max_changes_per_window":1}}`,
		"negative hygiene changes": `{"max_rows_rewritten":1,
			"hygiene":{"max_tables_per_window":1,"max_changes_per_window":-3}}`,
		"unknown hygiene field": `{"max_rows_rewritten":1,
			"hygiene":{"max_tables":1}}`,
		"string limit": `{"max_rows_rewritten":1,
			"hygiene":{"max_tables_per_window":"ten"}}`,
		"unknown budget kind": `{"max_rows_rewritten":1,
			"maintenance":{"max_tables_per_window":1}}`,
		"negative rows": `{"max_rows_rewritten":-1}`,
	}
	for name, blast := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDocument([]byte(policyWithBlastRadius(blast, "")))
			if err == nil {
				t.Fatalf("ParseDocument accepted %s", blast)
			}
			if !strings.HasPrefix(name, "unknown") && !strings.HasPrefix(name, "string") &&
				!strings.Contains(err.Error(), "negative") {
				t.Fatalf("err = %v, want a negative-limit error", err)
			}
		})
	}
	doc := UnattendedProfile()
	doc.BlastRadius.Hygiene.MaxTablesPerWindow = -1
	if err := ValidateDocument(doc); err == nil || !strings.Contains(err.Error(), "hygiene") {
		t.Fatalf("ValidateDocument(negative hygiene) = %v, want a hygiene error", err)
	}
}

// The profiles split today's envelope (20 tables, 50 changes, 5M rows per
// 24 hours) between the two kinds instead of adding to it.
func TestProfilesSplitTodaysEnvelope(t *testing.T) {
	for name, doc := range map[string]Document{
		"staffed": StaffedProfile(), "unattended": UnattendedProfile(),
	} {
		perf, hygiene := doc.Budget(BudgetPerformance), doc.Budget(BudgetHygiene)
		if perf.MaxTablesPerWindow+hygiene.MaxTablesPerWindow != 20 ||
			perf.MaxChangesPerWindow+hygiene.MaxChangesPerWindow != 50 {
			t.Errorf("%s: performance %+v + hygiene %+v, want 20 tables and 50 changes",
				name, perf, hygiene)
		}
		if perf.MaxTablesPerWindow <= 0 || hygiene.MaxTablesPerWindow <= 0 ||
			perf.MaxChangesPerWindow <= 0 || hygiene.MaxChangesPerWindow <= 0 {
			t.Errorf("%s: a kind has no budget: %+v / %+v", name, perf, hygiene)
		}
		if doc.BlastRadius.MaxRowsRewritten != 5000000 {
			t.Errorf("%s: max_rows_rewritten = %d", name, doc.BlastRadius.MaxRowsRewritten)
		}
	}
}

// An unknown kind reads the performance budget (fail closed).
func TestBudgetOfUnknownKindIsPerformance(t *testing.T) {
	doc := mustParse(t, lifeosStoredPolicy)
	if got := doc.Budget(BudgetKind("maintenance")); got != doc.Budget(BudgetPerformance) {
		t.Fatalf("unknown kind budget = %+v, want the performance budget", got)
	}
	if got := doc.Budget(""); got != doc.Budget(BudgetPerformance) {
		t.Fatalf("empty kind budget = %+v, want the performance budget", got)
	}
}
