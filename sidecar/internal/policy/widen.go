package policy

import "fmt"

// Widens reports whether next grants anything base does not (AGENTDB-SPEC
// §6.11: a policy proposal that widens needs two people when an agent
// proposed it). It is conservative: any change it cannot show to be equal
// or tighter counts as widening.
func Widens(base, next Document) bool {
	return profileWidens(base.Profile, next.Profile) ||
		!subsetClasses(next.AllowedChangeClasses, base.AllowedChangeClasses) ||
		!subsetClasses(base.ApprovalRequiredClasses, next.ApprovalRequiredClasses) ||
		!subsetStrings(next.MaintenanceWindows, base.MaintenanceWindows) ||
		ceilingWidens(base.LockDurationCeilingMS, next.LockDurationCeilingMS) ||
		limitsWiden(base, next) ||
		budgetsWiden(base.Budgets, next.Budgets) ||
		overridesWiden(base.DeadlineOverrides, next.DeadlineOverrides) ||
		!subsetStrings(base.RefusalSet, next.RefusalSet) ||
		base.UnknownClassification != next.UnknownClassification ||
		base.SerializeMode != next.SerializeMode
}

// WidensJSON compares two stored documents. A next document that does not
// parse is an error; a base that does not parse counts as widening (with
// the error), so an unreadable current policy never lowers the quorum.
func WidensJSON(base, next []byte) (bool, error) {
	nextDoc, err := ParseDocument(next)
	if err != nil {
		return true, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	baseDoc, err := ParseDocument(base)
	if err != nil {
		return true, fmt.Errorf("current policy unreadable: %w", err)
	}
	return Widens(baseDoc, nextDoc), nil
}

func profileWidens(base, next Profile) bool {
	return base != next && next != ProfileStaffed
}

// ceilingWidens: a higher ceiling, or none (0) where there was one.
func ceilingWidens(base, next int64) bool {
	if base == 0 {
		return false
	}
	return next == 0 || next > base
}

func limitsWiden(base, next Document) bool {
	b, n := base.BlastRadius, next.BlastRadius
	return n.MaxRowsRewritten > b.MaxRowsRewritten ||
		n.MaxTablesPerWindow > b.MaxTablesPerWindow ||
		n.Hygiene.MaxTablesPerWindow > b.Hygiene.MaxTablesPerWindow ||
		n.Hygiene.MaxChangesPerWindow > b.Hygiene.MaxChangesPerWindow ||
		next.RateLimits.MaxSelfInitiatedChangesPerWindow >
			base.RateLimits.MaxSelfInitiatedChangesPerWindow
}

func budgetsWiden(base, next Budgets) bool {
	return limitWidens(base.StorageBytes, next.StorageBytes) ||
		limitWidens(base.SpendDaily, next.SpendDaily) ||
		limitWidens(base.LLMTokensDaily, next.LLMTokensDaily)
}

// limitWidens: a capped limit raised, uncapped or dropped. An uncapped
// base cannot widen; an absent base widens unless next sets a cap.
func limitWidens(base, next BudgetLimit) bool {
	if base.NoCap() {
		return false
	}
	nextValue, nextCapped := next.Value()
	baseValue, baseCapped := base.Value()
	if !baseCapped {
		return !nextCapped && next.Present != base.Present
	}
	return !nextCapped || nextValue > baseValue
}

func overridesWiden(base, next map[DeadlineKind]bool) bool {
	for kind, on := range next {
		if on && !base[kind] {
			return true
		}
	}
	return false
}

func subsetClasses(sub, super []ChangeClass) bool {
	for _, class := range sub {
		if !containsChangeClass(super, class) {
			return false
		}
	}
	return true
}

func subsetStrings(sub, super []string) bool {
	have := make(map[string]bool, len(super))
	for _, s := range super {
		have[s] = true
	}
	for _, s := range sub {
		if !have[s] {
			return false
		}
	}
	return true
}
