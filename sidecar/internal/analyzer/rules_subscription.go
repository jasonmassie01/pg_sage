package analyzer

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// Why the subscription SQL in detail.manual_sql is never run unattended.
const (
	subSkipApproval   = "skipping a transaction loses its changes on the subscriber"
	subEnableApproval = "re-enabling retries the failing transaction; resolve the " +
		"conflict on the subscriber first"
)

// hasSubscriptions needs this cycle's subscriptions read (none is an empty,
// read category).
func hasSubscriptions(cur, _ *collector.Snapshot) bool {
	return cur.Subscriptions != nil && cur.Available("subscriptions")
}

// hasSlotList needs this cycle's slot read: no slot at all is a reading.
func hasSlotList(cur, _ *collector.Snapshot) bool { return cur.Available("replication") }

// holdOpen records identities of category a rule could not decide this
// cycle; runSnapshotRules keeps their open findings open.
func (e *RuleExtras) holdOpen(category string, idents ...string) {
	if e == nil {
		return
	}
	if e.undecided == nil {
		e.undecided = map[string][]string{}
	}
	e.undecided[category] = append(e.undecided[category], idents...)
}

// takeUndecided returns and clears the identities held open so far.
func (e *RuleExtras) takeUndecided() map[string][]string {
	if e == nil {
		return nil
	}
	out := e.undecided
	e.undecided = nil
	return out
}

func subIdent(name string) string { return "subscription:" + name }

// subErrorTotal is apply plus sync errors; ok is false before PG15.
func subErrorTotal(s collector.SubscriptionInfo) (int64, bool) {
	if s.ApplyErrorCount == nil && s.SyncErrorCount == nil {
		return 0, false
	}
	var n int64
	if s.ApplyErrorCount != nil {
		n += *s.ApplyErrorCount
	}
	if s.SyncErrorCount != nil {
		n += *s.SyncErrorCount
	}
	return n, true
}

// ruleSubscriptionErrors flags a subscription whose apply or sync error
// counters rose since the analyzer last read them (warning: the apply
// worker keeps failing, on a unique violation for example), and a disabled
// subscription with errors (critical: disable_on_error switched it off).
// The baseline lives in memory: a subscription first seen after a restart
// is undecided, so its open finding is kept until the next cycle.
func ruleSubscriptionErrors(
	cur, _ *collector.Snapshot, _ *config.Config, extras *RuleExtras,
) []Finding {
	if !hasSubscriptions(cur, nil) {
		return nil // unread: keep the baseline for the next read
	}
	seen := map[string]int64{}
	if extras != nil && extras.subErrorsSeen != nil {
		seen = extras.subErrorsSeen
	}
	next := map[string]int64{}
	var out []Finding
	for _, s := range cur.Subscriptions {
		total, ok := subErrorTotal(s)
		if !ok {
			continue
		}
		next[s.Name] = total
		last, known := seen[s.Name]
		delta := total - last
		if delta < 0 { // a stats reset: the errors since it count
			delta = total
		}
		switch {
		case !s.Enabled && total > 0:
			out = append(out, subscriptionErrorFinding(s, total, delta, known))
		case !s.Enabled:
		case !known:
			extras.holdOpen("subscription_errors", subIdent(s.Name))
		case delta > 0:
			out = append(out, subscriptionErrorFinding(s, total, delta, known))
		}
	}
	if extras != nil {
		extras.subErrorsSeen = next
	}
	return out
}

func subscriptionErrorFinding(
	s collector.SubscriptionInfo, total, delta int64, known bool,
) Finding {
	d := subscriptionDetail(s)
	d["error_count"] = total
	if known {
		d["error_count_delta"] = delta
	}
	quoted := sanitize.QuoteIdentifier(s.Name)
	f := Finding{
		Category: "subscription_errors", Severity: "warning",
		ObjectType: "subscription", ObjectIdentifier: subIdent(s.Name),
		Detail: d,
		// Advisory only: skipping loses data and re-enabling replays the
		// failure, so no SQL is ever run for this finding.
		ActionRisk: "high_risk",
	}
	if !s.Enabled {
		f.Severity = "critical"
		f.Title = fmt.Sprintf("Subscription %s is disabled after %d errors", s.Name, total)
		d["manual_sql"] = "ALTER SUBSCRIPTION " + quoted + " ENABLE;"
		d[DetailApprovalRequired] = subEnableApproval
		f.Recommendation = "The subscription stopped replicating (disable_on_error " +
			"or an operator disabled it after apply errors). Find the failing " +
			"transaction in the subscriber's log, resolve the conflicting row, then " +
			"re-enable it by hand (detail.manual_sql) with approval."
		return f
	}
	f.Title = fmt.Sprintf("Subscription %s apply worker failing (%d new errors)", s.Name, delta)
	d["manual_sql"] = "ALTER SUBSCRIPTION " + quoted +
		" SKIP (lsn = '<finish LSN from the subscriber log>');"
	d[DetailApprovalRequired] = subSkipApproval
	f.Recommendation = "The apply worker keeps failing and retrying (a unique " +
		"violation or a missing row on the subscriber, for example), so changes " +
		"stop arriving. Fix the conflicting row on the subscriber; skip the " +
		"transaction (detail.manual_sql, with the LSN from the log) only with " +
		"approval, since its changes are then lost on the subscriber."
	return f
}

func subscriptionDetail(s collector.SubscriptionInfo) map[string]any {
	d := map[string]any{
		"subscription":   s.Name,
		"enabled":        s.Enabled,
		"worker_running": s.WorkerPID != nil,
	}
	if s.ApplyErrorCount != nil {
		d["apply_error_count"] = *s.ApplyErrorCount
	}
	if s.SyncErrorCount != nil {
		d["sync_error_count"] = *s.SyncErrorCount
	}
	if s.Conflicts != nil {
		d["conflicts"] = s.Conflicts
	}
	if s.LatestEndLSN != nil {
		d["latest_end_lsn"] = *s.LatestEndLSN
	}
	if s.LatestEndTime != nil {
		d["latest_end_time"] = s.LatestEndTime.UTC().Format(time.RFC3339)
	}
	return d
}

func subscriptionLagLimit(cfg *config.Config) time.Duration {
	m := config.DefaultSubscriptionLagMinutes
	if cfg != nil && cfg.Analyzer.SubscriptionLagMinutes > 0 {
		m = cfg.Analyzer.SubscriptionLagMinutes
	}
	return time.Duration(m) * time.Minute
}

// ruleSubscriptionLag flags an enabled subscription whose last report to
// its publisher (latest_end_time) is older than subscription_lag_minutes,
// or which had no apply worker in this and the previous snapshot (a failed
// worker is restarted within seconds, so one snapshot without one is not
// evidence). An undecided subscription keeps its open finding.
func ruleSubscriptionLag(
	cur, prev *collector.Snapshot, cfg *config.Config, extras *RuleExtras,
) []Finding {
	if !hasSubscriptions(cur, nil) {
		return nil
	}
	limit := subscriptionLagLimit(cfg)
	var out []Finding
	for _, s := range cur.Subscriptions {
		if !s.Enabled {
			continue // subscription_errors owns a disabled subscription
		}
		switch {
		case s.WorkerPID == nil && workerGoneBefore(prev, s.Name):
			out = append(out, subscriptionLagFinding(s, 0, false))
		case s.WorkerPID == nil || s.LatestEndTime == nil:
			extras.holdOpen("subscription_lag", subIdent(s.Name))
		case cur.CollectedAt.Sub(*s.LatestEndTime) >= limit:
			out = append(out, subscriptionLagFinding(s,
				cur.CollectedAt.Sub(*s.LatestEndTime), true))
		}
	}
	return out
}

// workerGoneBefore reports whether prev read the subscription enabled and
// without an apply worker.
func workerGoneBefore(prev *collector.Snapshot, name string) bool {
	if prev == nil || !hasSubscriptions(prev, nil) {
		return false
	}
	for _, s := range prev.Subscriptions {
		if s.Name == name {
			return s.Enabled && s.WorkerPID == nil
		}
	}
	return false
}

func subscriptionLagFinding(s collector.SubscriptionInfo, lag time.Duration,
	worker bool) Finding {
	d := subscriptionDetail(s)
	f := Finding{
		Category: "subscription_lag", Severity: "warning",
		ObjectType: "subscription", ObjectIdentifier: subIdent(s.Name),
		Detail: d, ActionRisk: "moderate",
	}
	if !worker {
		f.Title = fmt.Sprintf("Subscription %s is enabled but has no apply worker", s.Name)
		f.Recommendation = "No apply worker ran in two consecutive snapshots. Check " +
			"the subscriber's log for worker failures, max_logical_replication_workers " +
			"and max_worker_processes, and that the publisher is reachable."
		return f
	}
	d["lag_seconds"] = lag.Seconds()
	f.Title = fmt.Sprintf("Subscription %s last reported to its publisher %s ago",
		s.Name, lag.Round(time.Second))
	f.Recommendation = "The subscriber has not confirmed WAL to its publisher " +
		"recently: check the network path, the publisher's walsender, and the " +
		"apply worker's progress in the subscriber's log."
	return f
}
