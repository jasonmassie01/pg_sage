package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// No concurrent access tests: the handler serves the read-only registry.

// rules07Lists are the source filter lists rules-07 shipped, copied here so
// the registry cannot change what an existing filter value returns.
var rules07Lists = map[string][]string{
	"rules": {"index_health", "unused_index", "invalid_index",
		"duplicate_index", "missing_fk_index", "slow_query",
		"high_plan_time", "query_regression", "seq_scan_heavy",
		"high_total_time", "temp_spill", "lock_chain", "connection_leak",
		"cache_hit_ratio", "checkpoint_pressure",
		"stat_statements_pressure", "replication_lag",
		"inactive_slot", "slow_replication_slot",
		"sequence_exhaustion", "sort_without_index",
		"work_mem_promotion", "table_bloat", "xid_wraparound",
		"extension_drift", "plan_regression", "wraparound_freeze",
		"autovacuum_tuning", "sage_footprint", "sage_self_cost",
		"sage_self_budget", "clone_schemas", "app_managed_index",
		"stale_statistics",
		"index_bloat", // rules-13 added index_bloat to the rules subsystem
		// market-13 added the subscriber and CDC health categories.
		"subscription_errors", "subscription_lag", "cdc_consumer_stalled",
		"cdc_heartbeat_stale",
		// market-07 added the WAL archiver rules.
		"wal_archive_failing", "wal_archive_stale",
		// market-02 added the seasonal workload anomaly.
		"workload_anomaly"},
	"advisor": {"wal_tuning", "connection_tuning", "query_rewrite",
		"bloat_remediation", "advisor_degraded", "vacuum_tuning", "memory_tuning",
		// market-15 added the PostgreSQL 18 I/O settings check.
		"io_settings"},
	"optimizer": {"missing_index", "covering_index", "partial_index", "composite_index"},
	// tuning-11 added query_parameterization to the query_tuning subsystem.
	"query_tuning": {"query_tuning", "stale_statistics", "runaway_query",
		"query_parameterization",
		// market-08 added the workload budget categories.
		"workload_budget_violation", "workload_budget_proposal", "workload_budget_enforce"},
	"incident": {"incident", "incident_open", "incident_resolved"},
}

func TestRegistryKeepsTheRules07FilterLists(t *testing.T) {
	for source, want := range rules07Lists {
		_, args, ok := buildSourceClause(source, 1)
		if !ok || len(args) != 1 {
			t.Fatalf("source %q = (%v, %v), want one list", source, args, ok)
		}
		got := slices.Clone(args[0].([]string))
		slices.Sort(got)
		w := slices.Clone(want)
		slices.Sort(w)
		if !slices.Equal(got, w) {
			t.Errorf("source %q matches %v, want the rules-07 list %v", source, got, w)
		}
	}
	clause, args, ok := buildSourceClause("schema_lint", 3)
	if !ok || clause != " AND category LIKE $3" || len(args) != 1 || args[0] != "schema_lint:%" {
		t.Errorf("schema_lint = (%q, %v, %v), want the prefix match", clause, args, ok)
	}
}

func TestNewSubsystemsFilterTheirCategories(t *testing.T) {
	clause, args, ok := buildSourceClause("governance", 2)
	if !ok || clause != " AND (category LIKE $2 OR category = ANY($3::text[]))" {
		t.Fatalf("governance clause = (%q, %v)", clause, ok)
	}
	if args[0] != "agent_posture:%" || !slices.Contains(args[1].([]string), "audit_chain_broken") {
		t.Errorf("governance args = %v", args)
	}
	_, args, ok = buildSourceClause("cloud", 1)
	if !ok || !slices.Equal(args[0].([]string),
		[]string{"backup_retention_below_policy", "backup_stale", // market-07
			"instance_rightsize", // market-04
			"managed_parameter_drift", "managed_storage_runway"}) {
		t.Errorf("cloud = (%v, %v), want the cloud categories", args, ok)
	}
}

func TestSubsystemFromCategoryUsesTheRegistry(t *testing.T) {
	cases := map[string]string{"bogus": "", "": "", "audit_chain_broken": "governance",
		"tuning_index_drop": "tuning", "managed_storage_runway": "cloud",
		"agent_posture:x": "governance", "migration_safety": "migration"}
	for c, want := range cases {
		if got := subsystemFromCategory(c); got != want {
			t.Errorf("subsystemFromCategory(%q) = %q, want %q", c, got, want)
		}
	}
	for _, c := range findingcat.All() {
		if c.Prefix {
			continue
		}
		if got := subsystemFromCategory(c.Name); got != c.Subsystem {
			t.Errorf("subsystemFromCategory(%q) = %q, want %q", c.Name, got, c.Subsystem)
		}
	}
}

type categoriesBody struct {
	Subsystems []struct {
		Name       string   `json:"name"`
		Categories []string `json:"categories"`
	} `json:"subsystems"`
	Categories []struct {
		Name      string   `json:"name"`
		Prefix    bool     `json:"prefix"`
		Subsystem string   `json:"subsystem"`
		AlsoIn    []string `json:"also_in"`
		Producer  string   `json:"producer"`
	} `json:"categories"`
}

func TestFindingCategoriesHandlerListsEveryFilterSubsystem(t *testing.T) {
	rec := httptest.NewRecorder()
	findingCategoriesHandler(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/findings/categories", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var body categoriesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(body.Categories) != len(findingcat.All()) {
		t.Fatalf("categories = %d, want %d", len(body.Categories), len(findingcat.All()))
	}
	var names []string
	for _, s := range body.Subsystems {
		names = append(names, s.Name)
		if _, _, ok := buildSourceClause(s.Name, 1); !ok {
			t.Errorf("listed subsystem %q is not accepted by the source filter", s.Name)
		}
		if len(s.Categories) == 0 {
			t.Errorf("subsystem %q lists no category", s.Name)
		}
	}
	for source := range rules07Lists {
		if !slices.Contains(names, source) {
			t.Errorf("subsystems %v miss filter value %q", names, source)
		}
	}
	for _, s := range []string{"forecaster", "schema_lint"} {
		if !slices.Contains(names, s) {
			t.Errorf("subsystems %v miss %q", names, s)
		}
	}
	if slices.Contains(names, "migration_advisor") {
		t.Error("the reserved migration_advisor filter must not be listed")
	}
}

func TestFindingCategoriesRouteIsViewerReadable(t *testing.T) {
	if !slices.ContainsFunc(routeProbes, func(p routeProbe) bool {
		return p.method == "GET" && p.path == "/api/v1/findings/categories"
	}) {
		t.Fatal("GET /api/v1/findings/categories is not in the route probe table")
	}
}
