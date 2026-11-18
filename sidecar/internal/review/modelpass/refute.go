package modelpass

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// runRefute is consensus "refute": run 1 ranks, run 2 sees run 1's kept
// items and must refute each with cited evidence; a refutation whose
// claims survive the filter drops the item. Only these two runs are made
// whatever review.model_runs says (above 1). A refutation run that does
// not finish fails the pass: one unchecked run is not a review.
func (p *pass) runRefute(ctx context.Context) passResult {
	res := passResult{}
	if p.turnedOff() {
		res.runs = []runOutcome{{attempt: 1, role: RoleRank, status: StatusSkipped,
			reason: reasonTurnedOff}}
		res.status, res.reason = report.PassSkipped, reasonTurnedOff
		return res
	}
	first := runOnce(ctx, p.d.Model, 1, RoleRank, p.rankConfig())
	res.runs = []runOutcome{first}
	answers := p.check(res.runs)
	if len(answers) == 0 {
		res.status, res.reason = p.shortfall(res.runs, 0)
		return res
	}
	m := majority(answers, 1)
	second := p.refuteRun(ctx, m)
	res.runs = append(res.runs, second)
	if second.status != StatusDone {
		res.status = report.PassFailed
		if second.status == StatusSkipped {
			res.status = report.PassSkipped
		}
		res.reason = "the refutation run did not finish: " + second.reason
		res.done = 1
		return res
	}
	if err := p.applyRefutations(&res.runs[1], &m); err != nil {
		res.status, res.reason, res.done = report.PassFailed, err.Error(), 1
		return res
	}
	res.status, res.done, res.merged = report.PassDone, 2, m
	return res
}

func (p *pass) refuteRun(ctx context.Context, m merged) runOutcome {
	if p.turnedOff() {
		return runOutcome{attempt: 2, role: RoleRefute, status: StatusSkipped,
			reason: reasonTurnedOff}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Review of %q. Another review kept these candidates (candidate_id, "+
		"its alias, rank). Refute each one you can with cited evidence; leave out the ones "+
		"you cannot refute.", p.in.Database)
	alias := map[string]int{}
	for i, c := range p.in.Candidates {
		alias[c.ID] = i + 1
	}
	for _, k := range m.kept {
		fmt.Fprintf(&b, "\n- %s is E%d, rank %d", k.id, alias[k.id], k.median)
	}
	cfg := p.config(b.String(), agentloop.Final{Name: refuteName, Parameters: refuteSchema,
		Description: "Submit one refutation per candidate you refute, each with claims " +
			"citing evidence aliases."})
	return runOnce(ctx, p.d.Model, 2, RoleRefute, cfg)
}

// applyRefutations drops each kept item a refutation with a surviving
// claim attacks.
func (p *pass) applyRefutations(o *runOutcome, m *merged) error {
	var r refutations
	if err := json.Unmarshal(o.res.Final, &r); err != nil {
		o.status = StatusFailed
		o.reason = fmt.Sprintf("the %s answer does not decode: %v", refuteName, err)
		return fmt.Errorf("the refutation run's answer does not decode: %v", err)
	}
	c := &checker{attempt: o.attempt, evidence: o.res.Evidence, known: p.known()}
	kept := map[string]bool{}
	for _, k := range m.kept {
		kept[k.id] = true
	}
	for _, ref := range r.Refutations {
		if !kept[ref.CandidateID] {
			c.reject(ref.CandidateID, "refutes no kept candidate")
			continue
		}
		claims := c.claims(ref.Claims)
		if len(claims) == 0 {
			c.reject(ref.CandidateID, "uncited refutation")
			continue
		}
		why := c.prose(ref.Argument, claims)
		if why == "" {
			why = "cited evidence"
		}
		m.refuted[ref.CandidateID] = p.ren.show(why)
		delete(kept, ref.CandidateID)
	}
	var left []keptItem
	for _, k := range m.kept {
		if kept[k.id] {
			left = append(left, k)
		}
	}
	m.kept = left
	m.rejected = append(m.rejected, c.rejected...)
	m.dropped = append(append(m.dropped, o.res.Dropped...), c.dropped...)
	p.kept[o.attempt], p.droppedOf[o.attempt] = c.kept, c.dropped
	return nil
}
