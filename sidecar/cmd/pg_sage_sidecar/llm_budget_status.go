package main

import (
	"time"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/llm"
)

// TokenStatus reports every tracked client, keyed "general"/"optimizer"
// for process-wide clients and "<database>/general|optimizer" for
// per-database ones, plus "<database>/fleet_budget" for each fleet budget
// share. The API status endpoint used to see only the shared client
// (G3-B14).
func (r *llmClientRegistry) TokenStatus() map[string]llm.ClientStatus {
	r.mu.Lock()
	entries := append([]llmRegistryEntry(nil), r.entries...)
	r.mu.Unlock()
	status := make(map[string]llm.ClientStatus, len(entries))
	for _, entry := range entries {
		status[llmStatusKey(entry)] = llm.StatusOf(entry.client)
	}
	addFleetBudgetStatus(status)
	return status
}

// ResetBudgets zeroes every tracked client and the fleet budget.
func (r *llmClientRegistry) ResetBudgets() {
	r.mu.Lock()
	entries := append([]llmRegistryEntry(nil), r.entries...)
	r.mu.Unlock()
	for _, entry := range entries {
		entry.client.ResetBudget()
	}
	if fleetLLMBudget != nil {
		fleetLLMBudget.ResetDaily()
	}
}

func llmStatusKey(entry llmRegistryEntry) string {
	role := "general"
	if entry.role == llmRoleOptimizer {
		role = "optimizer"
	}
	if entry.database == "" {
		return role
	}
	return entry.database + "/" + role
}

func addFleetBudgetStatus(status map[string]llm.ClientStatus) {
	if fleetLLMBudget == nil {
		return
	}
	now := time.Now().UTC()
	resets := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0,
		time.UTC).Format(time.RFC3339)
	for database, usage := range fleetLLMBudget.Snapshot() {
		status[database+"/fleet_budget"] = llm.ClientStatus{
			Model:          "fleet_budget",
			Enabled:        true,
			TokensUsed:     int64(usage.Used),
			TokenBudget:    usage.Allocation,
			Exhausted:      usage.Used >= usage.Allocation,
			ResetTimestamp: resets,
		}
	}
}

// llmBudgetRegistry is the API's view of every LLM client, or nil when the
// process has no LLM runtime (the API then reports an empty state).
func llmBudgetRegistry() api.LLMBudgetRegistry {
	if llmMgr == nil {
		return nil
	}
	return llmClients
}
