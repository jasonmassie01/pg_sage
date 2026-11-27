package analyzer

import (
	"context"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/notify"
)

// findingAlerter is a dispatcher that throttles finding alerts itself
// (notify's outbox state: cooldown, quiet hours, reminders).
type findingAlerter interface {
	AlertsFindings() bool
}

func (a *Analyzer) alertsFindings() bool {
	fa, ok := a.dispatcher.(findingAlerter)
	return ok && fa.AlertsFindings()
}

// dispatchFindingAlerts sends finding_opened for every finding that
// opened, reopened or escalated this cycle, at any severity (alerting-06).
func (a *Analyzer) dispatchFindingAlerts(ctx context.Context, res UpsertResult) {
	if a.dispatcher == nil || !a.alertsFindings() {
		return
	}
	for _, list := range [][]Finding{res.Opened, res.Reopened, res.Escalated} {
		for _, f := range list {
			detail, _ := json.Marshal(f.Detail)
			evt := notify.FindingOpenedEvent(f.Title, string(detail), a.databaseName,
				f.Category, f.ObjectIdentifier, f.Severity)
			if err := a.dispatcher.Dispatch(ctx, evt); err != nil {
				a.logFn("ERROR", "finding alert dispatch %s/%s: %v",
					f.Category, f.ObjectIdentifier, err)
			}
		}
	}
}

// dispatchFindingResolved sends finding_resolved for every cleared
// finding; the alert state sends it only to channels that were alerted.
func (a *Analyzer) dispatchFindingResolved(ctx context.Context, cleared []ClearedFinding) {
	if a.dispatcher == nil || !a.alertsFindings() {
		return
	}
	for _, c := range cleared {
		evt := notify.FindingResolvedEvent(c.Title, a.databaseName, c.Category,
			c.ObjectIdentifier, c.Severity)
		if err := a.dispatcher.Dispatch(ctx, evt); err != nil {
			a.logFn("ERROR", "finding resolve dispatch %s/%s: %v",
				c.Category, c.ObjectIdentifier, err)
		}
	}
}
