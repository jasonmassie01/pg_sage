package tuning

import (
	"context"
	"math"
	"slices"
	"sort"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// defaultMaxNewPerTable bounds new index proposals per table per cycle.
const defaultMaxNewPerTable = 3

// Ranking tiers: calibrated at or above the threshold (by the interval's
// lower bound), uncalibrated, calibrated below it.
const (
	tierConfident = iota
	tierUncalibrated
	tierDoubtful
)

type ranked struct {
	j    Judged
	tier int
	conf float64
}

// rank records each admitted proposal's calibrated confidence, orders
// them by tier, confidence, case weight and predicted size, and keeps the
// best within the per-table index cap and the cycle's proposal cap.
func (a *Agent) rank(judged []Judged, cal Calibration) []Judged {
	threshold := a.settings.ConfidenceThreshold
	rs := make([]ranked, 0, len(judged))
	for _, j := range judged {
		pct := 0.0
		if j.Prediction.ExpectedChangePct != nil {
			pct = *j.Prediction.ExpectedChangePct
		}
		j.Confidence = cal.ConfidenceFor(j.Class, j.Prediction.Method, pct)
		applyConfidence(j.Finding, j.Confidence, cal.MinOutcomes, threshold,
			j.Verdict == VerdictRedirected)
		r := ranked{j: j, tier: tierUncalibrated}
		if j.Confidence.Status == StatusCalibrated {
			r.conf, r.tier = *j.Confidence.Value, tierDoubtful
			if j.Confidence.WilsonLow >= threshold {
				r.tier = tierConfident
			}
		}
		rs = append(rs, r)
	}
	sort.SliceStable(rs, func(i, k int) bool { return better(rs[i], rs[k]) })
	return a.capped(rs)
}

func better(x, y ranked) bool {
	if x.tier != y.tier {
		return x.tier < y.tier
	}
	if x.conf != y.conf {
		return x.conf > y.conf
	}
	if x.j.Case.Weight != y.j.Case.Weight {
		return x.j.Case.Weight > y.j.Case.Weight
	}
	return magnitude(x.j) > magnitude(y.j)
}

func magnitude(j Judged) float64 {
	if j.Prediction.ExpectedChangePct == nil {
		return 0
	}
	return math.Abs(*j.Prediction.ExpectedChangePct)
}

// capped applies llm.optimizer.max_new_per_table to index creates and
// tuning.max_proposals_per_cycle to everything.
func (a *Agent) capped(rs []ranked) []Judged {
	perTable := a.settings.MaxNewPerTable
	if perTable <= 0 {
		perTable = defaultMaxNewPerTable
	}
	limit := a.settings.Tuning.MaxProposalsPerCycle
	indexes := map[string]int{}
	var out []Judged
	cut := 0
	for _, r := range rs {
		if r.j.Proposal.Type == ProposeIndexCreate && len(r.j.Tables) > 0 {
			if indexes[r.j.Tables[0]] >= perTable {
				cut++
				continue
			}
			indexes[r.j.Tables[0]]++
		}
		if limit > 0 && len(out) >= limit {
			cut++
			continue
		}
		out = append(out, r.j)
	}
	if cut > 0 {
		a.cappedN.Add(int64(cut))
		a.logFn("INFO", "tuning: %d admitted proposal(s) beyond the cap (%d per cycle, "+
			"%d new indexes per table) wait for a later cycle", cut, limit, perTable)
	}
	return out
}

// indexTables are the tables the findings propose indexes on, for the
// tuner to defer.
func indexTables(fs []analyzer.Finding) []string {
	var out []string
	for _, f := range fs {
		if !optimizer.IsOptimizerFinding(f.Category, f.Detail) {
			continue
		}
		if t := analyzer.OptimizerFindingTable(f); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// findSnapshotTable is the snapshot's row for a canonical table name.
func findSnapshotTable(snap *collector.Snapshot, name string) (collector.TableStats, bool) {
	if snap == nil {
		return collector.TableStats{}, false
	}
	for _, t := range snap.Tables {
		if qualified(t.SchemaName, t.RelName) == name {
			return t, true
		}
	}
	return collector.TableStats{}, false
}

// reloptions are a table's storage parameters, "" when none.
func reloptions(snap *collector.Snapshot, ts collector.TableStats) string {
	if snap == nil || snap.ConfigData == nil {
		return ""
	}
	for _, ro := range snap.ConfigData.TableReloptions {
		if ro.SchemaName == ts.SchemaName && ro.RelName == ts.RelName {
			return ro.Reloptions
		}
	}
	return ""
}

// recordHints records the kept query hints with the tuner (nothing is
// recorded before the cap) and returns the kept findings. A hint the tuner
// will not record, such as a statement hinted since it was checked, is
// dropped.
func (a *Agent) recordHints(ctx context.Context, kept []Judged) []analyzer.Finding {
	out := make([]analyzer.Finding, 0, len(kept))
	for _, j := range kept {
		if j.Hint != nil {
			if err := a.deps.Hints.RecordHint(ctx, *j.Hint); err != nil {
				a.logFn("WARN", "tuning: hint for queryid %d not recorded, dropped: %v",
					j.Hint.QueryID, err)
				continue
			}
		}
		out = append(out, *j.Finding)
	}
	return out
}
