package fleet

// BudgetSource reports each database's share of the fleet LLM token budget
// (fleet-06). *FleetBudget implements it; a nil *FleetBudget reports none.
type BudgetSource interface {
	Snapshot() map[string]BudgetUsage
}

// SetBudgetSource sets the fleet budget the status rows report; nil means
// no fleet budget is configured and the rows omit llm_budget.
func (m *DatabaseManager) SetBudgetSource(src BudgetSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budget = src
}

// budgetSnapshotLocked reads the budget once per status read, so every row
// of one overview comes from the same split. Caller holds m.mu (read or
// write); the budget takes only its own lock and never calls back.
func (m *DatabaseManager) budgetSnapshotLocked() map[string]BudgetUsage {
	if m.budget == nil {
		return nil
	}
	return m.budget.Snapshot()
}

// budgetFor is the database's usage from a snapshot, or nil when the
// database has no share in it.
func budgetFor(budgets map[string]BudgetUsage, name string) *BudgetUsage {
	usage, ok := budgets[name]
	if !ok {
		return nil
	}
	return &usage
}
