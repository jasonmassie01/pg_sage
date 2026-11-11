package main

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
)

// observability-04: what the executor did, for alerting. The two counters
// are process memory (they reset on restart; alert with increase()). The
// two gauges come from each database's action ledger through its metrics
// refresher, never from the scrape.

// metricsSourceActions is the refresh source of the action gauges.
const metricsSourceActions = "actions"

// actionMetricsSource reads the pending approvals and the last failure.
func actionMetricsSource(p *pgxpool.Pool) metricsSource {
	return metricsSource{name: metricsSourceActions,
		read: func(ctx context.Context, s *metricsSnapshot) error {
			if p == nil {
				return errNoMetricsPool
			}
			g, err := executor.ReadActionGauges(ctx, p)
			if err != nil {
				return err
			}
			s.actions = g
			return nil
		}}
}

// writeActionMetrics renders the outcome and verdict counters and, for
// each database whose last actions refresh succeeded, its gauges.
func writeActionMetrics(b *strings.Builder, outcomes []executor.OutcomeCount,
	verdicts []executor.VerdictCount, views []metricsView) {
	b.WriteString("# HELP pg_sage_actions_total Actions that ended, by change class " +
		"and outcome (success, failed, rolled_back, cancelled, unverifiable, other)\n" +
		"# TYPE pg_sage_actions_total counter\n")
	for _, o := range outcomes {
		writeSample(b, "pg_sage_actions_total", []labelPair{{"database", o.Database},
			{"class", o.Class}, {"outcome", o.Outcome}}, o.Count)
	}
	b.WriteString("\n# HELP pg_sage_verification_verdicts_total Verifications " +
		"finalized, by change class and verdict (success, revert, unverifiable, " +
		"failed, other)\n# TYPE pg_sage_verification_verdicts_total counter\n")
	for _, v := range verdicts {
		writeSample(b, "pg_sage_verification_verdicts_total",
			[]labelPair{{"database", v.Database}, {"class", v.Class},
				{"verdict", v.Verdict}}, v.Count)
	}
	writeActionGauges(b, views)
}

func writeActionGauges(b *strings.Builder, views []metricsView) {
	b.WriteString("\n# HELP pg_sage_actions_pending_approval Pending approvals a " +
		"person can act on now (past cooldown, not expired), by risk\n" +
		"# TYPE pg_sage_actions_pending_approval gauge\n")
	for _, v := range views {
		if !v.ok(metricsSourceActions) {
			continue
		}
		pending := v.snap.actions.PendingByRisk
		risks := make([]string, 0, len(pending))
		for risk := range pending {
			risks = append(risks, risk)
		}
		sort.Strings(risks)
		for _, risk := range risks {
			writeSample(b, "pg_sage_actions_pending_approval",
				[]labelPair{{"database", v.name}, {"risk", risk}}, pending[risk])
		}
	}
	b.WriteString("\n# HELP pg_sage_action_last_failure_timestamp_seconds When the " +
		"latest failed action was recorded (no sample: none has failed)\n" +
		"# TYPE pg_sage_action_last_failure_timestamp_seconds gauge\n")
	for _, v := range views {
		if !v.ok(metricsSourceActions) || v.snap.actions.LastFailure.IsZero() {
			continue
		}
		writeSample(b, "pg_sage_action_last_failure_timestamp_seconds", dbLabel(v.name),
			v.snap.actions.LastFailure.Unix())
	}
	b.WriteString("\n")
}
