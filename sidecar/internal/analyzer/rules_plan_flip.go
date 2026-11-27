package analyzer

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sanitize"
)

// guard-02c: a plan regression whose plan hash changed after an ANALYZE
// (auto or manual, not pg_sage's) of a relation the new plan reads, between
// the two captures. Routine statistics refreshes caused the Neon (May 2025)
// and Clerk (February 2026) plan flips; one more ANALYZE fixed both.
const categoryPlanFlip = "plan_flip_after_analyze"

// Who ran the ANALYZE that preceded the flip.
const (
	analyzedByAuto   = "autoanalyze"
	analyzedByManual = "manual"
)

// reanalyzeCooldown is how long after pg_sage's ANALYZE of a relation no
// second automatic one is proposed for it.
const reanalyzeCooldown = 24 * time.Hour

// pg_sage stamps executed_at after its ANALYZE returns, so its own
// ANALYZE of a relation is the one whose action was recorded within
// [last_analyze - sageAnalyzeBefore, last_analyze + sageAnalyzeAfter].
const (
	sageAnalyzeBefore = time.Minute
	sageAnalyzeAfter  = 5 * time.Minute
)

// relStats is one relation of the new plan and its last ANALYZE times.
type relStats struct {
	Schema, Name                 string
	LastAnalyze, LastAutoanalyze *time.Time
}

// sageAnalyze is one ANALYZE pg_sage's executor ran: its target as
// written ("schema.table" or "table", lower case) and when.
type sageAnalyze struct {
	Target string
	At     time.Time
}

// names reports whether the action's target is schema.name. An
// unqualified target names every table of that name (fail closed: the
// ANALYZE is taken as pg_sage's).
func (s sageAnalyze) names(schema, name string) bool {
	if s.Target == "" {
		return false
	}
	name = strings.ToLower(name)
	if !strings.Contains(s.Target, ".") {
		return s.Target == name
	}
	return schema != "" && s.Target == strings.ToLower(schema)+"."+name
}

// flipCauseT is the ANALYZE a flip is attributed to.
type flipCauseT struct {
	Schema, Name string
	At           time.Time
	By           string
}

// Relation is the analyzed relation as "schema.name".
func (c flipCauseT) Relation() string { return c.Schema + "." + c.Name }

// flipCause is the latest ANALYZE, strictly between the captures, of a
// relation the new plan reads, that pg_sage did not run.
func flipCause(rels []relStats, sage []sageAnalyze, prevAt, curAt time.Time) (flipCauseT,
	bool) {
	var best flipCauseT
	found := false
	consider := func(r relStats, at *time.Time, by string) {
		if at == nil || !at.After(prevAt) || !at.Before(curAt) {
			return
		}
		if by == analyzedByManual && ranBySage(sage, r, *at) {
			return
		}
		if !found || at.After(best.At) {
			best, found = flipCauseT{Schema: r.Schema, Name: r.Name, At: *at, By: by}, true
		}
	}
	for _, r := range rels {
		consider(r, r.LastAutoanalyze, analyzedByAuto)
		consider(r, r.LastAnalyze, analyzedByManual)
	}
	return best, found
}

func ranBySage(sage []sageAnalyze, r relStats, at time.Time) bool {
	for _, s := range sage {
		if s.names(r.Schema, r.Name) && !s.At.Before(at.Add(-sageAnalyzeBefore)) &&
			!s.At.After(at.Add(sageAnalyzeAfter)) {
			return true
		}
	}
	return false
}

// flipHashes returns a regression's two fingerprints when they are of the
// same version and differ (addPlanHashes sets plan_changed only then).
func flipHashes(reg Finding) (prev, cur string, ok bool) {
	changed, _ := reg.Detail["plan_changed"].(bool)
	prev, _ = reg.Detail["previous_plan_hash"].(string)
	cur, _ = reg.Detail["current_plan_hash"].(string)
	if !changed || prev == "" || cur == "" || prev == cur ||
		hashVersion(prev) != hashVersion(cur) {
		return "", "", false
	}
	return prev, cur, true
}

// blocksPerCall is a query's shared blocks per call before and after a
// moment, from the hourly query rollup; nil when unknown.
type blocksPerCall struct {
	Before, After *float64
}

// buildPlanFlipFinding is the flip of reg's query attributed to c, with a
// re-ANALYZE of c's relation unless pg_sage analyzed it within the
// cooldown.
func buildPlanFlipFinding(reg Finding, p planPair, c flipCauseT, b blocksPerCall,
	sage []sageAnalyze, now time.Time) Finding {
	rel := c.Relation()
	d := map[string]any{"queryid": p.QueryID, "query": p.QueryText, "relation": rel,
		"analyzed_at": c.At.UTC().Format(time.RFC3339Nano), "analyzed_by": c.By,
		"previous_captured_at": p.PreviousAt.UTC().Format(time.RFC3339Nano),
		"current_captured_at":  p.CurrentAt.UTC().Format(time.RFC3339Nano)}
	for _, k := range []string{"previous_plan_hash", "current_plan_hash", "cost_ratio",
		"previous_summary", "current_summary", "node_changes"} {
		if v, ok := reg.Detail[k]; ok {
			d[k] = v
		}
	}
	if b.Before != nil {
		d["blocks_per_call_before"] = *b.Before
	}
	if b.After != nil {
		d["blocks_per_call_after"] = *b.After
	}
	f := Finding{Category: categoryPlanFlip, Severity: reg.Severity, ObjectType: "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", p.QueryID), Detail: d,
		Title: fmt.Sprintf("Plan flip after ANALYZE of %s for queryid %d", rel, p.QueryID),
		Recommendation: fmt.Sprintf("The plan of query %d changed after %s analyzed %s "+
			"at %s. Running ANALYZE again draws a new sample and often restores the "+
			"earlier plan; pg_sage verifies the plan and blocks per call afterwards.",
			p.QueryID, c.By, rel, c.At.UTC().Format(time.RFC3339))}
	if last, held := lastSageAnalyze(sage, c, now); held {
		d["reanalyze_held_until"] = last.Add(reanalyzeCooldown).UTC().Format(time.RFC3339)
		f.Recommendation += fmt.Sprintf(" pg_sage analyzed %s at %s, so no second "+
			"ANALYZE is proposed within 24 hours.", rel, last.UTC().Format(time.RFC3339))
		return f
	}
	f.RecommendedSQL = "ANALYZE " + sanitize.QuoteQualifiedName(c.Schema, c.Name) + ";"
	f.ActionRisk = "safe"
	return f
}

// lastSageAnalyze is pg_sage's latest ANALYZE of c's relation, and
// whether it is within the cooldown at now.
func lastSageAnalyze(sage []sageAnalyze, c flipCauseT, now time.Time) (time.Time, bool) {
	var last time.Time
	for _, s := range sage {
		if s.names(c.Schema, c.Name) && s.At.After(last) {
			last = s.At
		}
	}
	return last, !last.IsZero() && now.Sub(last) < reanalyzeCooldown
}
