package main

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/metricsreg"
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

// writeExecutorMetrics renders the executor's process-wide counters
// (parks, wait releases, record failures, outcomes and verdicts) and
// each database's action gauges, the owner brief's among them.
func writeExecutorMetrics(b *strings.Builder, views []metricsView) {
	writeParkMetrics(b, executor.ParkCounts(), executor.WaitReleaseCounts())
	writeRecordFailureMetrics(b, executor.RecordFailureCounts())
	writeActionMetrics(b, executor.OutcomeCounts(), executor.VerdictCounts(), views)
	writeOwnerBriefMetrics(b, views)
}

// writeActionMetrics renders the outcome and verdict counters and, for
// each database whose last actions refresh succeeded, its gauges.
func writeActionMetrics(b *strings.Builder, outcomes []executor.OutcomeCount,
	verdicts []executor.VerdictCount, views []metricsView) {
	metricsreg.Header(b, "pg_sage_actions_total")
	for _, o := range outcomes {
		writeSample(b, "pg_sage_actions_total", []labelPair{{"database", o.Database},
			{"class", o.Class}, {"outcome", o.Outcome}}, o.Count)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_verification_verdicts_total")
	for _, v := range verdicts {
		writeSample(b, "pg_sage_verification_verdicts_total",
			[]labelPair{{"database", v.Database}, {"class", v.Class},
				{"verdict", v.Verdict}}, v.Count)
	}
	writeActionGauges(b, views)
}

func writeActionGauges(b *strings.Builder, views []metricsView) {
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_actions_pending_approval")
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
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_action_last_failure_timestamp_seconds")
	for _, v := range views {
		if !v.ok(metricsSourceActions) || v.snap.actions.LastFailure.IsZero() {
			continue
		}
		writeSample(b, "pg_sage_action_last_failure_timestamp_seconds", dbLabel(v.name),
			v.snap.actions.LastFailure.Unix())
	}
	b.WriteString("\n")
}
