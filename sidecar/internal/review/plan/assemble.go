package plan

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/review/modelpass"
)

// maxGoalRunes is plan-01's goal bound.
const maxGoalRunes = 500

// assemble orders the proposals (generator steps in rank order, then
// model steps) under plan-02's rules: one step per overlapping statement
// set and the plan caps (indexes per table, one restart-bound change).
// A proposal that would break either waits for the next review.
func (b *builder) assemble() {
	var kept []proposal
	for _, p := range b.props {
		if n := overlapping(kept, p); n > 0 {
			b.nextReview(p, fmt.Sprintf("its statements (queryids %v) overlap step %d's; "+
				"one change per statement set is measured at a time, so it waits for the "+
				"next review", p.step.TargetQueryIDs, n))
			continue
		}
		p.step.Ordinal = len(kept) + 1
		p.info.Ordinal = p.step.Ordinal
		trial := changeplan.Plan{Steps: append(steps(kept), p.step)}
		if err := changeplan.CheckPlanCaps(trial, b.in.Caps, b.in.Settings); err != nil {
			b.nextReview(p, fmt.Sprintf("it would break a plan cap (%v), so it waits for the "+
				"next review", err))
			continue
		}
		b.label(&p)
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		b.res.Message = NoChangesProposed
		return
	}
	for _, p := range kept {
		b.res.Steps = append(b.res.Steps, p.info)
	}
	b.res.Plan = b.plan(kept)
}

// overlapping is the ordinal of the first kept step whose targets share a
// statement with p's, or 0.
func overlapping(kept []proposal, p proposal) int {
	for _, k := range kept {
		for _, id := range p.step.TargetQueryIDs {
			if slices.Contains(k.step.TargetQueryIDs, id) {
				return k.step.Ordinal
			}
		}
	}
	return 0
}

func steps(ps []proposal) []changeplan.Step {
	out := make([]changeplan.Step, 0, len(ps)+1)
	for _, p := range ps {
		out = append(out, p.step)
	}
	return out
}

func (b *builder) nextReview(p proposal, why string) {
	b.advise(Advice{Origin: p.info.Origin, OriginRef: p.info.OriginRef, Kind: p.info.Kind,
		Target: p.info.Target, Title: p.info.Title, Reason: ReasonNextReview, Detail: why,
		EvidenceIDs: p.info.EvidenceIDs})
}

// label says whether the step needs a person's own approval: a step spec
// section 3 never runs automatically, a generator candidate that asked
// for approval, and every model-origin step until its model holds G-M1.
func (b *builder) label(p *proposal) {
	p.info.Measurement = Measurement{Metric: p.step.Metric,
		Direction: p.step.ExpectedDirection, TargetQueryIDs: p.step.TargetQueryIDs,
		Setting: p.step.Setting, MinCalls: changeplan.DefaultMinCalls,
		MinSamples: changeplan.DefaultMinSamples, DeadlineMinutes: p.step.DeadlineMinutes}
	if needs, why := changeplan.NeedsOwnApproval(p.step, b.in.Settings); needs {
		p.info.ApprovalRequired, p.info.ApprovalReason = true, why
		return
	}
	if p.info.Origin == OriginModel && !b.in.ModelAuthority {
		p.info.ApprovalRequired = true
		p.info.ApprovalReason = fmt.Sprintf("%s: proposed by %s, which has no authority to "+
			"run steps on a plan approval (G-M1)", changeplan.ReasonModelOrigin, b.modelRef())
	}
}

// plan is the plan-01 plan of the kept proposals: origin model when any
// step is the model's, else generator.
func (b *builder) plan(kept []proposal) *changeplan.Plan {
	p := &changeplan.Plan{DatabaseID: b.in.DatabaseID, Origin: changeplan.OriginGenerator,
		OriginRef: fmt.Sprintf("review_run:%d", b.in.RunID), PackHash: b.in.Pack.ContentSHA256,
		Status: changeplan.StatusProposed, CreatedBy: CreatedBy, Steps: steps(kept)}
	var gen, model int
	for _, k := range kept {
		p.Scope.Objects = appendAll(p.Scope.Objects, k.tables)
		for _, id := range k.step.TargetQueryIDs {
			if !slices.Contains(p.Scope.QueryIDs, id) {
				p.Scope.QueryIDs = append(p.Scope.QueryIDs, id)
			}
		}
		if k.info.Origin == OriginModel {
			model++
		} else {
			gen++
		}
	}
	slices.Sort(p.Scope.Objects)
	slices.Sort(p.Scope.QueryIDs)
	if model > 0 {
		p.Origin, p.Model, p.PromptVersion = changeplan.OriginModel, b.modelRef(),
			modelpass.PromptVersion
	}
	p.Goal = clip(fmt.Sprintf("Sage Review of %s (run %d): %d generator and %d model "+
		"steps", b.in.Database, b.in.RunID, gen, model), maxGoalRunes)
	return p
}

func appendAll(xs, ys []string) []string {
	for _, y := range ys {
		xs = appendNew(xs, y)
	}
	return xs
}

// clip bounds s to n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}
