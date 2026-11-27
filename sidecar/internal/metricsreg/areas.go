package metricsreg

// The areas docs/metrics.md groups the families by.
const (
	AreaProcess      = "process"
	AreaAPI          = "api"
	AreaHealth       = "health"
	AreaCollector    = "collector"
	AreaFleet        = "fleet"
	AreaLLM          = "llm"
	AreaOptimizer    = "optimizer"
	AreaActions      = "actions"
	AreaValue        = "value"
	AreaSLO          = "slo"
	AreaSRE          = "sre"
	AreaSelf         = "self"
	AreaIntegrations = "integrations"
)

// Area is one docs/metrics.md section.
type Area struct {
	ID, Title, Intro string
}

var areas = []Area{
	{AreaProcess, "Process and scrape", "The build, the mode, the control database " +
		"connection, leases and how fresh each database's metrics refresh is."},
	{AreaAPI, "API and sign-in", "Login limiter, API TLS certificate and SSO discovery."},
	{AreaHealth, "Database health", "One series per monitored database, from the fleet " +
		"manager's in-memory status: reachability, health, trust, findings, first look."},
	{AreaCollector, "Collector", "The collector's breaker, freshness and failures per " +
		"database."},
	{AreaFleet, "Fleet", "Fleet totals and the fleet LLM token budget per database."},
	{AreaLLM, "LLM", "Every LLM client's calls, latency, tokens, state and daily token " +
		"use, by client and database."},
	{AreaOptimizer, "Optimizer and tuning agent", "Index recommendations, rejection " +
		"memory and the tuning agent's per-cycle budget."},
	{AreaActions, "Actions, verification and policy", "Action outcomes, pending " +
		"approvals, verification verdicts, policy parks and shadow decisions."},
	{AreaValue, "Value", "Verified toil saved and incidents avoided, from each " +
		"database's value ledger."},
	{AreaSLO, "SLOs", "Error-budget state and burn rate of each configured SLO."},
	{AreaSRE, "Sage SRE", "Probe slot queueing and the investigation backlog."},
	{AreaSelf, "Self cost and budget", "What pg_sage itself costs each database and the " +
		"process, against its declared budget; periodic loop health."},
	{AreaIntegrations, "Integrations", "Log watcher, pgaudit, audit chain verification " +
		"and SIEM export (fleet leader only), migration review and Ask Sage."},
}

// Areas returns the docs/metrics.md sections in order.
func Areas() []Area { return append([]Area(nil), areas...) }

// Retired is a family removed or renamed in a release, with what replaces
// it: a registered family, or a note when none does.
type Retired struct {
	Name        string
	Replacement string // a registered family, or "" with Note
	Note        string
	Release     string
}

// retired is the v2.6.0 rename release (observability-09).
var retired = []Retired{
	{Name: "pg_sage_findings_total", Replacement: "pg_sage_findings_open",
		Note: "per database and severity", Release: "v2.6.0"},
	{Name: "pg_sage_collector_last_run_timestamp",
		Replacement: "pg_sage_collector_last_success_timestamp_seconds",
		Note:        "per database", Release: "v2.6.0"},
	{Name: "pg_sage_fleet_instance_findings", Replacement: "pg_sage_findings_open",
		Note: "sum by database", Release: "v2.6.0"},
	{Name: "pg_sage_fleet_instance_health", Replacement: "pg_sage_database_health_score",
		Release: "v2.6.0"},
	{Name: "pg_sage_connections_total", Note: "use postgres_exporter's " +
		"pg_stat_activity_count", Release: "v2.6.0"},
	{Name: "pg_sage_cache_hit_ratio", Note: "use postgres_exporter's " +
		"pg_stat_database_blks_hit and pg_stat_database_blks_read", Release: "v2.6.0"},
	{Name: "pg_sage_optimizer_recommendations_total",
		Replacement: "pg_sage_optimizer_recommendations", Note: "renamed: a gauge, " +
			"not a counter", Release: "v2.6.0"},
	{Name: "pg_sage_fleet_findings_total", Replacement: "pg_sage_fleet_findings_open",
		Note: "renamed: a gauge, not a counter", Release: "v2.6.0"},
}

// RetiredFamilies returns the removed and renamed families.
func RetiredFamilies() []Retired { return append([]Retired(nil), retired...) }
