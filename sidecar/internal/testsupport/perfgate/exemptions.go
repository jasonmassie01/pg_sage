package perfgate

import (
	"fmt"
	"strings"
)

// hasTag reports whether query carries tag in its pg_sage comment
// (/* pg_sage <tag> v1 */).
func hasTag(query, tag string) bool {
	return strings.Contains(query, "/* pg_sage "+tag+" ")
}

// meanBudget is the mean gate B judges a statement against: the ceiling
// of the exemption whose tag it carries (and that tag), else the budget.
func meanBudget(query string, b Budgets) (float64, string) {
	for tag, ex := range b.MeanExempt {
		if hasTag(query, tag) {
			return ex.CeilingMs, tag
		}
	}
	return b.StatementMeanMs, ""
}

// exemptRows is the rows the phase's statements tagged tag affected.
func exemptRows(p Phase, tag string) int64 {
	var rows int64
	for _, s := range p.Statements {
		if hasTag(s.Query, tag) {
			rows += s.Rows
		}
	}
	return rows
}

// chargedUpdates is the updates and HOT updates gate F judges a table by:
// for an exempt table, its updates less the rows of the exempt statement
// (which is never HOT by design), with a note saying how many.
func chargedUpdates(p Phase, t TableDelta, b Budgets) (updates, hot int64, note string) {
	ex, ok := b.HotExempt[t.Name]
	if !ok {
		return t.Updates, t.HotUpdates, ""
	}
	rows := exemptRows(p, ex.Tag)
	updates = max(t.Updates-rows, 0)
	return updates, min(t.HotUpdates, updates),
		fmt.Sprintf("; %d updates by statements tagged %s not charged", rows, ex.Tag)
}
