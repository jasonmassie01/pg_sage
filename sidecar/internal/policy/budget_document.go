package policy

import "fmt"

// blastRadiusWire is blast_radius as documents spell it. The performance
// budget is max_tables_per_window here plus
// rate_limits.max_self_initiated_changes_per_window (the fields every
// sidecar version reads), or a performance block that must agree with
// them; the hygiene block is optional and defaults per field:
//
//	"blast_radius": {"max_rows_rewritten": 5000000, "max_tables_per_window": 10,
//	  "performance": {"max_tables_per_window": 10, "max_changes_per_window": 25},
//	  "hygiene":     {"max_tables_per_window": 10, "max_changes_per_window": 25}}
type blastRadiusWire struct {
	MaxRowsRewritten   int64           `json:"max_rows_rewritten"`
	MaxTablesPerWindow *int64          `json:"max_tables_per_window,omitempty"`
	Performance        *kindBudgetWire `json:"performance,omitempty"`
	Hygiene            *kindBudgetWire `json:"hygiene,omitempty"`
}

type kindBudgetWire struct {
	MaxTablesPerWindow  *int64 `json:"max_tables_per_window,omitempty"`
	MaxChangesPerWindow *int64 `json:"max_changes_per_window,omitempty"`
}

type rateLimitsWire struct {
	MaxSelfInitiatedChangesPerWindow *int64 `json:"max_self_initiated_changes_per_window,omitempty"`
}

// blastRadiusFromWire resolves the performance budget from its block or
// its legacy fields (which must agree when both are set) and the hygiene
// budget from its block, a missing field taking the hygiene default.
func blastRadiusFromWire(
	radius blastRadiusWire, rates rateLimitsWire,
) (BlastRadius, RateLimits, error) {
	performance := kindBudgetWire{}
	if radius.Performance != nil {
		performance = *radius.Performance
	}
	tables, err := agreeingLimit("max_tables_per_window", performance.MaxTablesPerWindow,
		"blast_radius.max_tables_per_window", radius.MaxTablesPerWindow)
	if err != nil {
		return BlastRadius{}, RateLimits{}, err
	}
	changes, err := agreeingLimit("max_changes_per_window", performance.MaxChangesPerWindow,
		"rate_limits.max_self_initiated_changes_per_window",
		rates.MaxSelfInitiatedChangesPerWindow)
	if err != nil {
		return BlastRadius{}, RateLimits{}, err
	}
	hygiene := DefaultHygieneBudget()
	if radius.Hygiene != nil {
		hygiene.MaxTablesPerWindow = valueOr(radius.Hygiene.MaxTablesPerWindow,
			hygiene.MaxTablesPerWindow)
		hygiene.MaxChangesPerWindow = valueOr(radius.Hygiene.MaxChangesPerWindow,
			hygiene.MaxChangesPerWindow)
	}
	return BlastRadius{
		MaxRowsRewritten: radius.MaxRowsRewritten, MaxTablesPerWindow: tables,
		Hygiene: hygiene,
	}, RateLimits{MaxSelfInitiatedChangesPerWindow: changes}, nil
}

// agreeingLimit reads a performance limit from its block or its legacy
// field. Both set to different values is a conflict, never a silent pick;
// neither set is zero, as a missing legacy field always was.
func agreeingLimit(name string, current *int64, legacyName string, legacy *int64,
) (int64, error) {
	if current != nil && legacy != nil && *current != *legacy {
		return 0, fmt.Errorf("blast_radius.performance.%s=%d conflicts with %s=%d",
			name, *current, legacyName, *legacy)
	}
	return valueOr(current, valueOr(legacy, 0)), nil
}

func valueOr(value *int64, fallback int64) int64 {
	if value == nil {
		return fallback
	}
	return *value
}

// blastRadiusJSON is the saved blast_radius of doc. It is downgrade-safe
// (owner decision 2026-10-03): the performance budget is written in the
// legacy fields an older sidecar reads, and the hygiene block, which an
// older sidecar's strict parser rejects, only when it is not the default.
func blastRadiusJSON(doc Document) map[string]any {
	radius := map[string]any{
		"max_rows_rewritten":    doc.BlastRadius.MaxRowsRewritten,
		"max_tables_per_window": doc.Budget(BudgetPerformance).MaxTablesPerWindow,
	}
	if doc.Budget(BudgetHygiene) != DefaultHygieneBudget() {
		radius["hygiene"] = doc.Budget(BudgetHygiene)
	}
	return radius
}

// rateLimitsJSON is the saved rate_limits: the performance change limit.
func rateLimitsJSON(doc Document) map[string]any {
	changes := doc.Budget(BudgetPerformance).MaxChangesPerWindow
	return map[string]any{"max_self_initiated_changes_per_window": changes}
}

func validateBlastRadius(doc Document) error {
	if doc.BlastRadius.MaxRowsRewritten < 0 {
		return fmt.Errorf("max_rows_rewritten cannot be negative")
	}
	for _, kind := range []BudgetKind{BudgetPerformance, BudgetHygiene} {
		budget := doc.Budget(kind)
		if budget.MaxTablesPerWindow < 0 {
			return fmt.Errorf("blast_radius.%s.max_tables_per_window cannot be negative", kind)
		}
		if budget.MaxChangesPerWindow < 0 {
			return fmt.Errorf("blast_radius.%s.max_changes_per_window cannot be negative",
				kind)
		}
	}
	return nil
}
