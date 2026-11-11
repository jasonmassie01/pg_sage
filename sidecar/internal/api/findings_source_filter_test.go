package api

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/findingcat"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// No concurrent access tests: the source clause, the subsystem mapping and
// the risk fields are pure functions over package-level read-only lists.

// sourceLists names each filterable category list and its subsystem, as
// the finding category registry declares them.
func sourceLists() map[string][]string {
	lists := map[string][]string{}
	for _, s := range []string{"rules", "advisor", "optimizer", "query_tuning", "incident"} {
		lists[s] = findingcat.Names(findingcat.BySubsystem(s))
	}
	return lists
}

// sharedCategories are emitted by two subsystems; the row's Source is the
// first one named here, and both filters match it.
var sharedCategories = map[string]string{"stale_statistics": "rules"}

func TestSourceListsMapToTheirOwnSubsystem(t *testing.T) {
	for subsystem, cats := range sourceLists() {
		if len(cats) == 0 {
			t.Fatalf("%s category list is empty", subsystem)
		}
		for _, c := range cats {
			want := subsystem
			if owner, ok := sharedCategories[c]; ok {
				want = owner
			}
			if got := subsystemFromCategory(c); got != want {
				t.Errorf("subsystemFromCategory(%q) = %q, want %q", c, got, want)
			}
		}
	}
}

// producerCategories are the categories each subsystem's producers emit,
// written out independently of the filter lists under test.
var producerCategories = map[string][]string{
	"advisor": {"wal_tuning", "connection_tuning", "query_rewrite",
		"bloat_remediation", "advisor_degraded", "vacuum_tuning", "memory_tuning"},
	"optimizer": {"missing_index", "covering_index", "partial_index",
		"composite_index"},
	"rules": {"wraparound_freeze", "autovacuum_tuning", "sage_footprint",
		"sage_self_cost", "sage_self_budget", "clone_schemas",
		"app_managed_index", "stale_statistics", "temp_spill", "duplicate_index"},
	"query_tuning": {"query_tuning", "stale_statistics", "runaway_query",
		"query_parameterization"},
	"incident": {"incident_open"},
}

func TestSourceListsHoldTheProducersCategories(t *testing.T) {
	want := producerCategories
	lists := sourceLists()
	for subsystem, cats := range want {
		for _, c := range cats {
			if !slices.Contains(lists[subsystem], c) {
				t.Errorf("%s filter misses producer category %q", subsystem, c)
			}
		}
	}
}

func TestSubsystemFromCategoryHappyAndUnknown(t *testing.T) {
	cases := map[string]string{
		"wraparound_freeze":      "rules",
		"wal_tuning":             "advisor",
		"advisor_degraded":       "advisor",
		"missing_index":          "optimizer",
		"composite_index":        "optimizer",
		"stale_statistics":       "rules",
		"runaway_query":          "query_tuning",
		"query_parameterization": "query_tuning",
		"forecast_disk":          "forecaster",
		"schema_lint:lint_x":     "schema_lint",
		"incident_open":          "incident",
		"":                       "",
		"advisor":                "",
		"optimizer":              "",
		"no_such_category_ever":  "",
	}
	for c, want := range cases {
		if got := subsystemFromCategory(c); got != want {
			t.Errorf("subsystemFromCategory(%q) = %q, want %q", c, got, want)
		}
	}
}

func TestBuildSourceClauseUsesCategoryLists(t *testing.T) {
	for _, source := range []string{"rules", "advisor", "optimizer",
		"query_tuning", "incident"} {
		clause, args, ok := buildSourceClause(source, 4)
		if !ok {
			t.Fatalf("source %q not recognised", source)
		}
		if clause != " AND category = ANY($4::text[])" {
			t.Errorf("source %q clause = %q", source, clause)
		}
		if strings.Contains(clause, "subsystem") {
			t.Errorf("source %q still reads detail->>'subsystem': %q", source, clause)
		}
		got, isList := args[0].([]string)
		if len(args) != 1 || !isList || !slices.Equal(got, sourceLists()[source]) {
			t.Errorf("source %q args = %v, want its category list", source, args)
		}
	}
}

func TestBuildSourceClauseStaleStatisticsInBothFilters(t *testing.T) {
	for _, source := range []string{"rules", "query_tuning"} {
		_, args, _ := buildSourceClause(source, 1)
		if !slices.Contains(args[0].([]string), "stale_statistics") {
			t.Errorf("%s filter hides stale_statistics", source)
		}
	}
}

func TestBuildSourceClauseUnknownAndReserved(t *testing.T) {
	for _, source := range []string{"", "bogus", "Rules", "advisor;--"} {
		if clause, args, ok := buildSourceClause(source, 1); ok || clause != "" || args != nil {
			t.Errorf("source %q = (%q, %v, %v), want no filter", source, clause, args, ok)
		}
	}
	clause, args, ok := buildSourceClause("migration_advisor", 1)
	if !ok || clause != " AND 1=0" || args != nil {
		t.Errorf("migration_advisor = (%q, %v, %v), want an empty result", clause, args, ok)
	}
}

func TestFindingRiskFieldsSubsetDropIsHigh(t *testing.T) {
	got := findingRiskFields(`DROP INDEX CONCURRENTLY "public"."ix_sub"`,
		map[string]any{"action_risk": "high_risk"})
	if got["action_risk"] != "high" || got["contract_tier"] != "moderate" ||
		got["action_type"] != "drop_unused_index" {
		t.Fatalf("subset drop risk = %v, want high over a moderate contract", got)
	}
	if got["approval_required"] != true || got["approval_reason"] == "" {
		t.Fatalf("a high-risk drop must show approval required: %v", got)
	}
}

func TestFindingRiskFieldsEmptySQLIsAdvisory(t *testing.T) {
	for _, detail := range []any{nil, map[string]any{}, "corrupt",
		map[string]any{"action_risk": "safe"}} {
		got := findingRiskFields("", detail)
		if got["contract_tier"] != "none" || got["action_risk"] != "none" ||
			got["action_type"] != "" || got["approval_required"] != false {
			t.Fatalf("empty SQL (detail %v) = %v, want advisory only", detail, got)
		}
	}
}

func TestFindingRiskFieldsBoundaries(t *testing.T) {
	got := findingRiskFields("ANALYZE public.t", map[string]any{"action_risk": "safe"})
	if got["action_risk"] != "safe" || got["contract_tier"] != "safe" ||
		got["approval_required"] != false || got["approval_reason"] != "" {
		t.Fatalf("ANALYZE = %v, want safe with no approval", got)
	}
	got = findingRiskFields("ALTER SYSTEM SET shared_buffers = '2GB'", nil)
	if got["approval_required"] != true ||
		!strings.Contains(got["approval_reason"].(string), "restart") ||
		got["action_risk"] != "moderate" {
		t.Fatalf("shared_buffers = %v, want moderate needing approval (restart)", got)
	}
}

func TestFindingRiskFieldsStricterOfProducerAndContract(t *testing.T) {
	// The contract wins over a laxer producer label.
	got := findingRiskFields(`DROP INDEX CONCURRENTLY public.ix`,
		map[string]any{"action_risk": "safe"})
	if got["action_risk"] != "moderate" {
		t.Fatalf("safe producer over moderate contract = %v, want moderate", got)
	}
	// An unrecognised producer label fails closed to high, as the policy
	// gate treats an unknown tier.
	got = findingRiskFields("ANALYZE t", map[string]any{"action_risk": "weird"})
	if got["action_risk"] != "high" || got["approval_required"] != true {
		t.Fatalf("unknown producer risk = %v, want high needing approval", got)
	}
	// SQL with no contract keeps the producer's label, advisory only.
	got = findingRiskFields("SELECT 1", map[string]any{"action_risk": "moderate"})
	if got["contract_tier"] != "none" || got["action_risk"] != "moderate" {
		t.Fatalf("uncontracted SQL = %v, want tier none, producer moderate", got)
	}
	got = findingRiskFields("SELECT 1", nil)
	if got["contract_tier"] != "none" || got["action_risk"] != "none" {
		t.Fatalf("uncontracted SQL without producer risk = %v, want none", got)
	}
}

func TestBuildFindingMapCarriesRiskFields(t *testing.T) {
	sql := `DROP INDEX CONCURRENTLY "public"."ix_sub"`
	now := time.Now()
	row := buildFindingMapWithAction(7, now, now, 1, "duplicate_index", "info",
		nil, nil, "Subset index", []byte(`{"action_risk":"high_risk"}`), nil, &sql,
		nil, "open", "db", nil, nil, nil, nil, nil)
	if row["action_risk"] != "high" || row["contract_tier"] != "moderate" ||
		row["approval_required"] != true || row["subsystem"] != "rules" {
		t.Fatalf("list row = %v, want rules subset drop at high risk", row)
	}
	row = buildFindingMapWithAction(8, now, now, 1, "wal_tuning", "info",
		nil, nil, "WAL", []byte(`not json`), nil, nil,
		nil, "open", "db", nil, nil, nil, nil, nil)
	if row["contract_tier"] != "none" || row["action_risk"] != "none" ||
		row["subsystem"] != "advisor" {
		t.Fatalf("advisory row = %v, want advisor with tier none", row)
	}
}

// forecast-06: storage_forecast came from the retired v0.9 growth
// forecaster, which never ran; every forecaster category now carries the
// forecast prefix, so the source filter is that prefix alone.
func TestForecasterSourceIsTheForecastPrefix(t *testing.T) {
	if got := subsystemFromCategory("forecast_disk_growth"); got != "forecaster" {
		t.Fatalf("subsystemFromCategory(forecast_disk_growth) = %q, want forecaster", got)
	}
	if got := subsystemFromCategory("storage_forecast"); got == "forecaster" {
		t.Fatalf("the retired storage_forecast still maps to %q", got)
	}
	clause, args, ok := buildSourceClause("forecaster", 2)
	if !ok || clause != " AND category LIKE $2" {
		t.Fatalf("forecaster clause = %q (ok %v)", clause, ok)
	}
	if len(args) != 1 || args[0] != "forecast%" {
		t.Fatalf("forecaster args = %v", args)
	}
	where, all := buildFindingsWhere(fleet.FindingFilters{Source: "forecaster",
		ThematicCategory: "indexing"})
	if !strings.Contains(where, "detail->>'thematic_category' = $2") || len(all) != 2 {
		t.Fatalf("placeholders after the forecaster arg are off: %q %v", where, all)
	}
}
