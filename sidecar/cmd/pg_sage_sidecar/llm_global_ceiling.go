package main

import (
	"strconv"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Sources of the process-wide LLM ceiling, as /api/v1/llm/status reports.
const (
	globalSourceConfigured = "configured" // llm.global_token_budget_daily
	globalSourceFleet      = "fleet"      // llm.fleet_token_budget_daily
	globalSourceDerived    = "derived"    // sum of every client's budget
)

// deriveGlobalCeiling picks the process-wide ceiling: an explicit
// llm.global_token_budget_daily, else today's effective total. That total
// is the sum of every registry client's own daily budget, except that
// while a fleet budget is active (fleetTotal > 0) the per-database clients
// it scopes count as its total instead; the shared process clients keep
// their own budgets on top, as before. Any unlimited counted client makes
// the derived ceiling unlimited (0), so by default the ceiling changes no
// spend. fleet_token_budget_daily without an active fleet budget (a
// standalone sidecar, where the key is inert) does not lower it.
func deriveGlobalCeiling(
	parent config.LLMConfig, entries []llmRegistryEntry, fleetTotal int,
) (int64, string) {
	if parent.GlobalTokenBudgetDaily > 0 {
		return int64(parent.GlobalTokenBudgetDaily), globalSourceConfigured
	}
	source, sum := globalSourceDerived, int64(0)
	if fleetTotal > 0 {
		source, sum = globalSourceFleet, int64(fleetTotal)
	}
	for _, entry := range entries {
		if fleetTotal > 0 && entry.database != "" {
			continue // charged to, and bounded by, the fleet budget
		}
		budget := derivedLLMConfig(parent, entry.role, entry.allowed).TokenBudgetDaily
		if budget <= 0 {
			return 0, source
		}
		sum += int64(budget)
	}
	return sum, source
}

// activeFleetTotal is the running fleet budget's daily total, 0 when no
// fleet budget scopes the per-database clients.
func activeFleetTotal() int {
	if fleetLLMBudget == nil {
		return 0
	}
	return fleetLLMBudget.TotalDaily
}

// refreshGlobalLocked recomputes the ceiling after the registry or its
// parent config changed, and logs it when it moves. Caller holds r.mu.
func (r *llmClientRegistry) refreshGlobalLocked() {
	if r.global == nil {
		r.global = llm.NewBudget()
	}
	ceiling, source := deriveGlobalCeiling(r.parentLocked(), r.entries, activeFleetTotal())
	before := r.global.Status()
	r.global.SetCeiling(ceiling, source)
	if before.Ceiling == ceiling && before.Source == source && r.globalLogged {
		return
	}
	r.globalLogged = true
	limit := "unlimited"
	if ceiling > 0 {
		limit = formatTokensPerDay(ceiling)
	}
	logInfo("llm", "global LLM token ceiling: %s (%s from %d clients)",
		limit, globalCeilingDerivation(source), len(r.entries))
}

func formatTokensPerDay(tokens int64) string {
	return strconv.FormatInt(tokens, 10) + " tokens/day"
}

func globalCeilingDerivation(source string) string {
	switch source {
	case globalSourceConfigured:
		return "llm.global_token_budget_daily"
	case globalSourceFleet:
		return "llm.fleet_token_budget_daily plus the shared clients' token_budget_daily"
	}
	return "derived: sum of token_budget_daily"
}

// attachGlobalLocked charges the client to the shared ceiling. Caller
// holds r.mu.
func (r *llmClientRegistry) attachGlobalLocked(client *llm.Client) {
	if r.global == nil {
		r.global = llm.NewBudget()
	}
	client.SetGlobalBudget(r.global)
}

// GlobalBudget reports the process-wide ceiling for /api/v1/llm/status.
func (r *llmClientRegistry) GlobalBudget() llm.BudgetStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.global.Status()
}
