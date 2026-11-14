package shadow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// maintenanceSettle is how old a manual VACUUM or ANALYZE must be before
// it is taken as run outside pg_sage: pg_sage logs its own runs once they
// end, so a run that just ended may still be its own.
const maintenanceSettle = time.Minute

// maintenanceSignalSQL reads, for a table ($1): whether it is gone, whether
// a manual VACUUM ($2 true) or ANALYZE ran after the decision was recorded
// ($3) and at least the settle delay ago ($5 seconds), whether the
// database's statistics were reset since, and the decision's metric ($4)
// now. Autovacuum's runs (last_autovacuum, last_autoanalyze) never count.
const maintenanceSignalSQL = `/* pg_sage */ SELECT r.oid IS NULL,
	COALESCE(x.ran > $3 AND x.ran <= now() - make_interval(secs => $5), false),
	COALESCE((SELECT d.stats_reset > $3 FROM pg_stat_database d
	          WHERE d.datname = current_database()), false),
	CASE $4::text
	  WHEN '` + verify.MetricDeadTuples + `' THEN t.n_dead_tup::float8
	  WHEN '` + verify.MetricModsSinceAnalyze + `' THEN t.n_mod_since_analyze::float8
	  WHEN '` + verify.MetricFrozenXIDAge + `' THEN age(c.relfrozenxid)::float8 END
	FROM (SELECT to_regclass($1) AS oid) r
	LEFT JOIN pg_class c ON c.oid = r.oid
	LEFT JOIN pg_stat_user_tables t ON t.relid = r.oid
	CROSS JOIN LATERAL (SELECT CASE WHEN $2::bool THEN t.last_vacuum
	                                ELSE t.last_analyze END AS ran) x`

// maintenanceSignal is one bounded read of a table's maintenance state.
type maintenanceSignal struct {
	gone, applied, reset bool
	metric               *float64
}

// maintenanceExternal is the verdict of a manual VACUUM or ANALYZE of
// p's table run outside pg_sage since p was recorded: the decision's
// baseline against the metric read now, in the pass that first sees it.
func (s *Scorer) maintenanceExternal(ctx context.Context, p pendingDecision) (*verifiedFact,
	bool, error) {
	table := maintenanceTable(p.Decision)
	if table == "" {
		return &verifiedFact{Unscorable: "the decision names no table"}, false, nil
	}
	resolved, err := s.appName(ctx, table)
	if err != nil {
		return catalogFailure("resolve "+table, err)
	}
	var sig maintenanceSignal
	err = s.catalog.QueryRow(ctx, maintenanceSignalSQL, resolved, p.Class == "vacuum",
		p.RecordedAt, p.Prediction.Metric, s.settle.Seconds()).Scan(&sig.gone, &sig.applied,
		&sig.reset, &sig.metric)
	switch {
	case err != nil:
		return catalogFailure("read the maintenance statistics of "+table, err)
	case sig.gone:
		return relationGone(table), false, nil
	case sig.reset:
		return &verifiedFact{Unscorable: "statistics were reset after the decision " +
			"was recorded"}, false, nil
	case !sig.applied:
		return nil, false, nil
	}
	own, err := s.ownTableAction(ctx, p.Decision, table)
	if err != nil || own {
		return nil, own, err
	}
	if err := s.markApplied(ctx, p.ID); err != nil {
		return nil, false, err
	}
	return maintenanceFact(p.Decision, sig.metric), false, nil
}

// maintenanceTable is the table d maintains: the [schema.]name its SQL
// ends with, read as PostgreSQL reads it (a quoted name keeps its quotes),
// else its object.
func maintenanceTable(d Decision) string {
	toks := tokenize(d.SQL)
	for len(toks) > 0 && toks[len(toks)-1].text == ";" {
		toks = toks[:len(toks)-1]
	}
	n := len(toks)
	if n < 2 || toks[n-1].punct {
		return strings.TrimSpace(d.Object)
	}
	start := n - 1
	if n >= 4 && toks[n-2].punct && toks[n-2].text == "." && !toks[n-3].punct {
		start = n - 3
	}
	return join(toks[start:])
}

// maintenanceFact judges a run found outside pg_sage: final at once, by
// the bar of pg_sage's own maintenance. Without a recorded baseline it is
// unverifiable, so it is never counted.
func maintenanceFact(d Decision, after *float64) *verifiedFact {
	metric := d.Prediction.Metric
	if d.Prediction.Baseline == nil {
		return &verifiedFact{Verdict: verify.OutcomeUnverifiable, Final: true,
			Reason:   fmt.Sprintf("the decision recorded no baseline of %q: not credited", metric),
			Evidence: map[string]any{"verdict": verify.OutcomeUnverifiable, "metric": metric}}
	}
	now := -1.0 // unreadable
	if after != nil {
		now = *after
	}
	j := verify.JudgeMaintenanceMetric(d.Class, metric, *d.Prediction.Baseline, now)
	return &verifiedFact{Verdict: j.Verdict, Final: true, Reason: j.Reason,
		Evidence: map[string]any{"verdict": j.Verdict, "metric": metric, "before": j.Before,
			"after": j.After, "change_pct": j.ObservedPct}}
}
