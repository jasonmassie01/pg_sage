package alerting

import (
	"context"
	"sort"
)

// knownSeverities is the fixed delivery order (alerting-05): a critical
// page never queues behind warning or info sends.
var knownSeverities = map[string]int{"critical": 0, "warning": 1, "info": 2}

// severityLess orders critical, warning, info, then any other severity
// by name, so delivery order is the same on every cycle.
func severityLess(a, b string) bool {
	ra, okA := knownSeverities[a]
	rb, okB := knownSeverities[b]
	switch {
	case okA && okB:
		return ra < rb
	case okA != okB:
		return okA
	default:
		return a < b
	}
}

// severityGroup is the findings of one severity, in input order.
type severityGroup struct {
	severity string
	findings []AlertFinding
}

// orderedSeverityGroups groups findings by severity, most severe group
// first. Each group keeps the input order: fresh findings by last_seen
// DESC, then pending ones (mergeFindings).
func orderedSeverityGroups(findings []AlertFinding) []severityGroup {
	groups := groupBySeverity(findings)
	out := make([]severityGroup, 0, len(groups))
	for sev, g := range groups {
		out = append(out, severityGroup{severity: sev, findings: g})
	}
	sort.Slice(out, func(i, j int) bool {
		return severityLess(out[i].severity, out[j].severity)
	})
	return out
}

// dispatchFindings sends every routed finding, one severity group after
// the other in orderedSeverityGroups order. Delivery stays serial.
func (m *Manager) dispatchFindings(ctx context.Context, findings []AlertFinding) {
	for _, g := range orderedSeverityGroups(findings) {
		if channels, ok := m.routes[g.severity]; ok {
			m.dispatchGroup(ctx, g.severity, g.findings, channels)
		}
	}
}

// orderResolveTargets puts resolve notifications in the same severity
// order as triggers, keeping row order (finding id) inside a severity.
func orderResolveTargets(targets []resolveTarget) {
	sort.SliceStable(targets, func(i, j int) bool {
		return severityLess(targets[i].finding.Severity, targets[j].finding.Severity)
	})
}
