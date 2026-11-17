package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/planfresh"
)

// planFlipDB reads the plan-flip rule's evidence (plan_flip_db.go).
type planFlipDB interface {
	// relations reads the last ANALYZE times of the plan's relations.
	relations(ctx context.Context, names planfresh.Names) ([]relStats, error)
	// sageAnalyzes reads pg_sage's recent ANALYZE actions and the
	// database clock.
	sageAnalyzes(ctx context.Context) ([]sageAnalyze, time.Time, error)
	// blocksPerCall reads the query's blocks per call before and after
	// the hour of pivot.
	blocksPerCall(ctx context.Context, queryid int64, pivot time.Time) (blocksPerCall, error)
	// openFlips reads the open flips and those pg_sage's ANALYZE resolved
	// that are not judged yet.
	openFlips(ctx context.Context) ([]openFlip, error)
	// latestCapture reads the query's newest plan fingerprint (nil: none).
	latestCapture(ctx context.Context, queryid int64) (*latestCapture, error)
	// recordVerdict merges detail into the finding's stored detail.
	recordVerdict(ctx context.Context, id int64, detail map[string]any) error
}

func (a *Analyzer) planFlips() planFlipDB {
	if a.flipDB != nil {
		return a.flipDB
	}
	return pgPlanFlipDB{pool: a.pool}
}

// checkPlanFlips detects plan flips among this cycle's plan pairs and
// follows up the open ones. Unread pairs or a failed read fail the
// category, so its open findings stay open.
func (a *Analyzer) checkPlanFlips(ctx context.Context, pairs []planPair,
	pairsRead bool) []Finding {
	if !pairsRead {
		a.evalFail(categoryPlanFlip)
		return nil
	}
	db := a.planFlips()
	sage, now, err := db.sageAnalyzes(ctx)
	if err != nil {
		a.evalFail(categoryPlanFlip)
		a.logFn("ERROR", "analyzer: %s: read pg_sage's ANALYZE actions: %v",
			categoryPlanFlip, err)
		return nil
	}
	detected := a.detectPlanFlips(ctx, db, pairs, sage, now)
	followed, err := a.followUpPlanFlips(ctx, db)
	if err != nil {
		a.evalFail(categoryPlanFlip)
		a.logFn("ERROR", "analyzer: %s: read open flips: %v", categoryPlanFlip, err)
		return detected
	}
	a.eval.evaluated(categoryPlanFlip)
	return mergeFlips(detected, followed)
}

// mergeFlips keeps a follow-up (it carries the re-ANALYZE's verdict) over
// a detection of the same query.
func mergeFlips(detected, followed []Finding) []Finding {
	byIdent := make(map[string]bool, len(followed))
	for _, f := range followed {
		byIdent[f.ObjectIdentifier] = true
	}
	out := append([]Finding(nil), followed...)
	for _, f := range detected {
		if !byIdent[f.ObjectIdentifier] {
			out = append(out, f)
		}
	}
	return out
}

// detectPlanFlips is the flip of each regressed pair whose plan hash
// changed. A pair whose relations cannot be read is left unevaluated: its
// open finding is kept.
func (a *Analyzer) detectPlanFlips(ctx context.Context, db planFlipDB, pairs []planPair,
	sage []sageAnalyze, now time.Time) []Finding {
	var out []Finding
	proposedBy := map[string]string{} // relation -> the flip carrying its ANALYZE
	for _, p := range pairs {
		reg := regressionOf(p)
		if reg == nil {
			continue
		}
		if _, _, ok := flipHashes(*reg); !ok {
			continue
		}
		f, err := a.planFlipOf(ctx, db, p, *reg, sage, now)
		if err != nil {
			a.eval.keep(categoryPlanFlip, reg.ObjectIdentifier)
			a.logFn("WARN", "analyzer: %s: query %d left unevaluated: %v",
				categoryPlanFlip, p.QueryID, err)
			continue
		}
		if f != nil {
			holdSecondProposal(f, proposedBy)
			out = append(out, *f)
		}
	}
	return out
}

// holdSecondProposal strips the ANALYZE from a flip whose relation an
// earlier flip of this cycle already proposes: one re-ANALYZE covers both.
func holdSecondProposal(f *Finding, proposedBy map[string]string) {
	if f.RecommendedSQL == "" {
		return
	}
	rel, _ := f.Detail["relation"].(string)
	by, seen := proposedBy[rel]
	if !seen {
		proposedBy[rel] = f.ObjectIdentifier
		return
	}
	f.RecommendedSQL, f.ActionRisk = "", ""
	f.Detail["reanalyze_covered_by"] = by
	f.Recommendation += " The re-ANALYZE proposed for " + by + " covers this query too."
}

func (a *Analyzer) planFlipOf(ctx context.Context, db planFlipDB, p planPair, reg Finding,
	sage []sageAnalyze, now time.Time) (*Finding, error) {
	names, err := planfresh.PlanNames(p.CurrentPlan)
	if err != nil {
		return nil, fmt.Errorf("name the new plan's relations: %w", err)
	}
	if names.Empty() {
		return nil, nil
	}
	rels, err := db.relations(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("read the last ANALYZE of %v: %w",
			append(names.Qualified, names.Unqualified...), err)
	}
	c, ok := flipCause(rels, sage, p.PreviousAt, p.CurrentAt)
	if !ok {
		return nil, nil
	}
	b, err := db.blocksPerCall(ctx, p.QueryID, c.At)
	if err != nil {
		a.logFn("WARN", "analyzer: %s: blocks per call of query %d unavailable: %v",
			categoryPlanFlip, p.QueryID, err)
		b = blocksPerCall{}
	}
	f := buildPlanFlipFinding(reg, p, c, b, sage, now)
	return &f, nil
}

// followUpPlanFlips judges each open flip from the query's newest plan:
// still flipped stays open, a re-ANALYZE that changed nothing is handed
// on (re-emitted without SQL), an improvement is recorded and resolves.
func (a *Analyzer) followUpPlanFlips(ctx context.Context, db planFlipDB) ([]Finding, error) {
	rows, err := db.openFlips(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, o := range rows {
		qid, _ := o.Detail["queryid"].(float64)
		if qid == 0 {
			a.keepOpenFlip(o)
			continue
		}
		latest, err := db.latestCapture(ctx, int64(qid))
		if err != nil {
			a.keepOpenFlip(o)
			a.logFn("WARN", "analyzer: %s: %s left unevaluated: %v", categoryPlanFlip,
				o.Ident, err)
			continue
		}
		blocks := a.blocksAfterReanalyze(ctx, db, o, int64(qid))
		switch v := judgeReanalyze(o, latest, blocks); v {
		case verdictNoChange:
			out = append(out, followUpFinding(o, v, latest, blocks))
		case verdictImproved:
			if err := db.recordVerdict(ctx, o.ID, verdictDetail(o, v, latest,
				blocks)); err != nil {
				a.logFn("WARN", "analyzer: %s: record the verdict of finding %d: %v",
					categoryPlanFlip, o.ID, err)
			}
		case verdictOpen, verdictPending:
			a.keepOpenFlip(o)
		}
	}
	return out, nil
}

func (a *Analyzer) keepOpenFlip(o openFlip) {
	if o.Status == "open" {
		a.eval.keep(categoryPlanFlip, o.Ident)
	}
}

// blocksAfterReanalyze is the query's blocks per call in the full hours
// after the re-ANALYZE, nil without one or when unknown.
func (a *Analyzer) blocksAfterReanalyze(ctx context.Context, db planFlipDB, o openFlip,
	qid int64) *float64 {
	if o.ActionID == 0 {
		return nil
	}
	b, err := db.blocksPerCall(ctx, qid, o.ActionAt.Add(time.Hour))
	if err != nil {
		a.logFn("WARN", "analyzer: %s: blocks per call of query %d after the "+
			"re-ANALYZE unavailable: %v", categoryPlanFlip, qid, err)
		return nil
	}
	return b.After
}
