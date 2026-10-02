package srebench

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Rules-only baselines for the runway families: naive first-match rules
// over the same evidence, with no nearness, consumption, cycling or
// pinning checks.

// ruleSeqFraction is the share of its limit at which the naive sequence
// rule blames the binding limit.
const ruleSeqFraction = 0.5

var ruleLimitNodes = map[string]string{probes.LimitSequenceType: "sequence_type_limit",
	probes.LimitColumnType:  "column_narrower_than_sequence",
	probes.LimitExplicitMax: "explicit_maxvalue_limit"}

func seqRule(ev []probes.Result, subject string) string {
	s := series(ev, probes.SequenceRunwayProbe)
	if len(s) == 0 {
		return ""
	}
	ss, err := probes.Sequences(s[len(s)-1])
	if err != nil {
		return ""
	}
	name := strings.TrimPrefix(subject, "sequence ")
	for _, q := range ss {
		if q.Sequence == name && q.Fraction >= ruleSeqFraction {
			return ruleLimitNodes[q.BindingLimit()]
		}
	}
	return ""
}

func hasHolder(hs []probes.XminHolder, kinds ...string) bool {
	for _, h := range hs {
		for _, k := range kinds {
			if h.Kind == k {
				return true
			}
		}
	}
	return false
}

func wrapRule(ev []probes.Result) string {
	if s := series(ev, probes.XminHorizon); len(s) > 0 {
		hs, err := probes.XminHolders(s[len(s)-1])
		switch {
		case err != nil:
		case hasHolder(hs, probes.HolderPreparedXact):
			return "xmin_held_by_prepared_xact"
		case hasHolder(hs, probes.HolderSlot, probes.HolderSlotCatalog, probes.HolderStandby):
			return "xmin_held_by_replication"
		case hasHolder(hs, probes.HolderSession):
			return "xmin_held_by_session"
		}
	}
	if s := series(ev, probes.XIDRunwayProbe); len(s) > 0 {
		if x, err := probes.XIDRunwayOf(s[len(s)-1]); err == nil && x.WorkersSaturated() {
			return "autovacuum_saturated"
		}
	}
	return ""
}

func diskRule(ev []probes.Result) string {
	if root := walRule(ev); root != "" {
		return root
	}
	s := series(ev, probes.RunwayTrendsProbe)
	if len(s) == 0 {
		return ""
	}
	ts, err := probes.RunwayTrends(s[len(s)-1])
	if err != nil {
		return ""
	}
	if tr, ok := probes.FindTrend(ts, probes.RunwayDatabaseBytes,
		probes.SubjectCluster); ok && tr.RatePerS > 0 {
		return "database_growth"
	}
	return ""
}
