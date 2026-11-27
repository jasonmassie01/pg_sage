package executor

import (
	"errors"
	"slices"

	prov "github.com/pg-sage/sidecar/internal/provider"
)

type ActionContract struct {
	ActionType          string
	BaseRiskTier        string
	ProviderSupport     []string
	RequiredPermissions []string
	Prechecks           []string
	Guardrails          []string
	ExecutionPlan       []string
	SuccessCriteria     []string
	PostChecks          []string
	RollbackClass       string
	Cooldown            string
	AuditFields         []string
	// Narrowing marks a contract that only takes access away (§6.2.4); the
	// gate lets it pass the hard stops, trust, budgets and windows.
	Narrowing bool
}

func (c ActionContract) Validate() error {
	if c.ActionType == "" || c.BaseRiskTier == "" {
		return errors.New("action contract missing type or risk tier")
	}
	if len(c.PostChecks) == 0 {
		return errors.New("action contract missing post-checks")
	}
	if c.RollbackClass == "" {
		return errors.New("action contract missing rollback class")
	}
	return nil
}

func AnalyzeTableContract() ActionContract {
	return ActionContract{
		ActionType:      "analyze_table",
		BaseRiskTier:    "safe",
		ProviderSupport: lakebaseOwnedActionProviders(),
		RequiredPermissions: []string{
			"table ownership or ANALYZE privilege",
		},
		Prechecks: []string{
			"table exists",
			"table is not excluded by policy",
			"emergency stop is not active",
			"stale-stat evidence exists",
			"cooldown window has elapsed",
		},
		Guardrails: []string{
			"dedicated connection",
			"statement_timeout",
			"per-table cooldown",
			"fleet analyze semaphore",
			"per-cluster safe-action concurrency limit",
		},
		ExecutionPlan: []string{"ANALYZE qualified_table"},
		SuccessCriteria: []string{
			"last_analyze advances or analyze_count increases",
			"stale-stat case no longer fires",
		},
		PostChecks: []string{
			"verify last_analyze or analyze_count changed",
			"rerun analyzer",
			"compare planner row-estimate error where available",
		},
		RollbackClass: "no_rollback_needed",
		Cooldown:      "configured analyze cooldown",
		AuditFields: []string{
			"table",
			"prior_last_analyze",
			"new_last_analyze",
			"prior_estimate_error",
			"post_action_estimate_error",
			"case_id",
		},
	}
}

// alterSystemGUCContract changes a GUC cluster-wide: self-managed only.
func alterSystemGUCContract(actionType string) ActionContract {
	return gucChangeContract(actionType, "ALTER SYSTEM", []string{"postgres"})
}

// alterDatabaseGUCContract changes a GUC for one database.
func alterDatabaseGUCContract(actionType string) ActionContract {
	return gucChangeContract(actionType, "ALTER DATABASE", databaseGUCProviders())
}

func gucChangeContract(
	actionType string,
	scope string,
	providers []string,
) ActionContract {
	return ActionContract{
		ActionType:          actionType,
		BaseRiskTier:        "moderate",
		ProviderSupport:     providers,
		RequiredPermissions: []string{"permission to change database configuration"},
		Prechecks: []string{
			"parameter is in the executor GUC allowlist",
			"current value is captured for rollback",
		},
		Guardrails: []string{
			"approval or autonomous moderate policy required",
			"single validated GUC change",
			"maintenance-window enforcement",
		},
		ExecutionPlan:   []string{scope + " SET or RESET validated_parameter"},
		SuccessCriteria: []string{"configured value matches the requested value"},
		PostChecks:      []string{"read back the effective and pending value"},
		RollbackClass:   "reversible",
		Cooldown:        "configured cascade cooldown",
		AuditFields:     []string{"database", "parameter", "old_value", "new_value"},
	}
}

func portableActionProviders() []string {
	return []string{prov.Postgres, prov.RDS, prov.Aurora, prov.CloudSQL, prov.AlloyDB,
		prov.Neon, prov.Supabase, prov.Azure, prov.AzureCosmos, prov.CNPG}
}

// databaseGUCProviders run ALTER DATABASE ... SET: not Cosmos DB for
// PostgreSQL, whose parameters are guidance-only (no pg_sage adapter).
func databaseGUCProviders() []string {
	return slices.DeleteFunc(lakebaseOwnedActionProviders(),
		func(name string) bool { return name == prov.AzureCosmos })
}

// lakebaseOwnedActionProviders is intentionally limited to the contracts whose
// owned-table SQL and production operator path are covered by tests/lakebaselive.
// Other portable contracts keep their existing provider list; a PostgreSQL wire
// connection alone does not establish provider feature or privilege support.
func lakebaseOwnedActionProviders() []string {
	return append(portableActionProviders(), prov.Lakebase)
}
