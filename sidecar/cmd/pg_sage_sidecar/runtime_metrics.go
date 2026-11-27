package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// modeGaugeValue encodes the operating mode for pg_sage_mode. Fleet used
// to report 0, indistinguishable from the meta-db mode (G10-B11). Meta
// keeps 0, the value it reported under the removed "extension" label.
func modeGaugeValue(mode string) int {
	switch mode {
	case "standalone":
		return 1
	case "fleet":
		return 2
	default:
		return 0
	}
}

func writeModeMetric(b *strings.Builder, mode string) {
	metricsreg.Header(b, "pg_sage_mode")
	fmt.Fprintf(b, "pg_sage_mode %d\n\n", modeGaugeValue(mode))
}

// writeLoginLimiterMetric exposes how many logins were refused because a
// login limiter was full of locked keys, a sign of credential spraying.
func writeLoginLimiterMetric(b *strings.Builder, saturated uint64) {
	metricsreg.Header(b, "pg_sage_login_limiter_saturated_total")
	fmt.Fprintf(b, "pg_sage_login_limiter_saturated_total %d\n\n", saturated)
}

// writeFleetBudgetMetrics exposes per-database fleet LLM budget spend so
// operators can see why LLM features stopped for one database (G3-B14,
// G5-D18). A nil budget (feature disabled) writes nothing.
func writeFleetBudgetMetrics(b *strings.Builder, budget *fleet.FleetBudget) {
	if budget == nil {
		return
	}
	snapshot := budget.Snapshot()
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	sort.Strings(names)
	metricsreg.Header(b, "pg_sage_llm_fleet_budget_used_tokens")
	for _, name := range names {
		writeSample(b, "pg_sage_llm_fleet_budget_used_tokens", dbLabel(name),
			snapshot[name].Used)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_llm_fleet_budget_allocation_tokens")
	for _, name := range names {
		writeSample(b, "pg_sage_llm_fleet_budget_allocation_tokens", dbLabel(name),
			snapshot[name].Allocation)
	}
	b.WriteString("\n")
}

// supervisorDeclared reports whether SAGE_SUPERVISED declares a process
// supervisor that relaunches on exit 42. Without one the UI restart would
// simply kill the sidecar (G5-B09).
func supervisorDeclared(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("SAGE_SUPERVISED"))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// forcedShutdownExitCode keeps the restart intent when graceful shutdown
// overruns its deadline.
func forcedShutdownExitCode() int {
	if restartRequested.Load() {
		return restartExitCode
	}
	return 1
}

// optimizerGaugeValue is 1 when the index optimizer can run: enabled and
// its LLM client (dedicated or general) configured. llm.optimizer.enabled
// defaults to true, so the config value alone does not mean it runs.
func optimizerGaugeValue() int {
	if !cfg.LLM.Optimizer.Enabled || llmMgr == nil {
		return 0
	}
	client := llmMgr.ForPurpose("index_optimization")
	if client == nil || !client.IsEnabled() {
		return 0
	}
	return 1
}

// writeSSODiscoveryMetrics exposes whether SSO discovery has succeeded and
// how many attempts failed (apiauth-07), so an IdP outage that keeps SSO
// unavailable pages someone. OAuth disabled writes nothing.
func writeSSODiscoveryMetrics(b *strings.Builder, st api.SSODiscoveryStatus) {
	if !st.Enabled {
		return
	}
	ready := 0
	if st.Ready {
		ready = 1
	}
	metricsreg.Header(b, "pg_sage_sso_discovery_ready")
	fmt.Fprintf(b, "pg_sage_sso_discovery_ready %d\n\n", ready)
	metricsreg.Header(b, "pg_sage_sso_discovery_failures_total")
	fmt.Fprintf(b, "pg_sage_sso_discovery_failures_total %d\n\n", st.Failures)
}
