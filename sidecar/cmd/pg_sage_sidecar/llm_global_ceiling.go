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
// llm.global_token_budget_daily, else llm.fleet_token_budget_daily, else
// the sum of every registry client's own daily budget. Any unlimited
// client makes the derived ceiling unlimited (0), so by default the
// ceiling only names today's effective total and changes no spend.
func deriveGlobalCeiling(
	parent config.LLMConfig, entries []llmRegistryEntry,
) (int64, string) {
	if parent.GlobalTokenBudgetDaily > 0 {
		return int64(parent.GlobalTokenBudgetDaily), globalSourceConfigured
	}
	if parent.FleetTokenBudgetDaily > 0 {
		return int64(parent.FleetTokenBudgetDaily), globalSourceFleet
	}
	var sum int64
	for _, entry := range entries {
		budget := derivedLLMConfig(parent, entry.role, entry.allowed).TokenBudgetDaily
		if budget <= 0 {
			return 0, globalSourceDerived
		}
		sum += int64(budget)
	}
	return sum, globalSourceDerived
}

// refreshGlobalLocked recomputes the ceiling after the registry or its
// parent config changed, and logs it when it moves. Caller holds r.mu.
func (r *llmClientRegistry) refreshGlobalLocked() {
	if r.global == nil {
		r.global = llm.NewBudget()
	}
	ceiling, source := deriveGlobalCeiling(r.parentLocked(), r.entries)
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
		return "llm.fleet_token_budget_daily"
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
