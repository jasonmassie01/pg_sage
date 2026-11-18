package plan

import (
	"errors"
	"math"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// generatorSteps turns each candidate the review kept (in rank order)
// into a proposal or advice. A ranked id with no candidate is ignored: the
// report cannot add a step the generators did not propose.
func (b *builder) generatorSteps() {
	byID := make(map[string]candidate.Candidate, len(b.in.Candidates))
	for _, c := range b.in.Candidates {
		byID[c.ID] = c
	}
	for _, r := range b.in.Report.Ranked {
		c, ok := byID[r.CandidateID]
		if !ok || (r.Decision != "" && r.Decision != report.DecisionKeep) {
			continue
		}
		p, err := b.fromCandidate(c)
		if err == nil {
			b.props = append(b.props, p)
			continue
		}
		var why *refusal
		if errors.As(err, &why) {
			b.advise(Advice{Origin: OriginGenerator, OriginRef: c.Generator, Kind: c.Kind,
				Target: c.Target, Title: c.Reason, Reason: why.reason, Detail: why.detail,
				EvidenceIDs: c.EvidenceIDs})
		}
	}
}

func (b *builder) fromCandidate(c candidate.Candidate) (proposal, error) {
	switch c.ActionType {
	case candidate.ActionAdvice, candidate.ActionOperator:
		detail := strings.TrimSpace(c.Reason)
		if detail == "" {
			detail = "the generator proposes this as advice for a person"
		}
		return proposal{}, refuse(ReasonAdviceOnly, "%s", detail)
	}
	if _, ok := executor.ContractForActionType(c.ActionType); !ok {
		return proposal{}, refuse(ReasonNoContract, "no executor contract for action type %s",
			c.ActionType)
	}
	r := rendered{sql: c.ForwardSQL, rollback: c.RollbackSQL}
	if err := checkSQL(r); err != nil {
		return proposal{}, err
	}
	b.measureCandidate(c, &r)
	if err := measurable(r); err != nil {
		return proposal{}, err
	}
	return b.candidateProposal(c, r), nil
}

// measureCandidate sets what a candidate's verdict measures: a setting
// reads back; an index drop watches the table's writers (write cost
// down), else its readers on the index keys (no regression); any other
// change the statements on the tables its evidence names.
func (b *builder) measureCandidate(c candidate.Candidate, r *rendered) {
	if st, ok := pgconf.ParseAlterSystem(c.ForwardSQL); ok {
		r.metric, r.direction, r.setting = changeplan.MetricReadback, changeplan.DirectionNone,
			st.Name
		return
	}
	if st, ok := pgconf.ParseAlterDatabase(c.ForwardSQL); ok {
		r.metric, r.direction, r.setting = changeplan.MetricReadback, changeplan.DirectionNone,
			st.Name
		return
	}
	var all []packQuery
	for _, s := range b.lp.subjectsOf(c.EvidenceIDs) {
		r.tables = appendNew(r.tables, s.schema+"."+s.table)
		qs := b.lp.tableQueries(s.schema, s.table)
		if c.ActionType == "drop_unused_index" {
			if w := writers(qs); len(w) > 0 {
				all = append(all, w...)
				r.metric, r.direction = changeplan.MetricWALPerTuple, changeplan.DirectionDown
				continue
			}
			qs = mentioning(qs, s.columns)
		}
		all = append(all, qs...)
	}
	r.targets = hottest(all)
	if r.metric != "" {
		return
	}
	switch {
	case c.ActionType == "drop_unused_index":
		r.metric, r.direction = changeplan.MetricTimePerCall, changeplan.DirectionNone
	case c.ActionType == "create_index_concurrently":
		r.metric, r.direction = changeplan.MetricBlocksPerCall, direction(c)
	default:
		r.metric, r.direction = changeplan.MetricTimePerCall, direction(c)
	}
}

func direction(c candidate.Candidate) changeplan.Direction {
	switch strings.ToLower(c.Prediction.Direction) {
	case "down", "decrease":
		return changeplan.DirectionDown
	case "up", "increase":
		return changeplan.DirectionUp
	}
	return changeplan.DirectionNone
}

func (b *builder) candidateProposal(c candidate.Candidate, r rendered) proposal {
	s := changeplan.Step{Kind: changeplan.KindAction, ActionType: c.ActionType,
		Setting: r.setting, ProposedSQL: c.ForwardSQL, RollbackSQL: c.RollbackSQL,
		EvidenceIDs: append([]string(nil), c.EvidenceIDs...), TargetQueryIDs: ids(r.targets),
		Metric: r.metric, ExpectedDirection: r.direction,
		ExpectedSource: sourceOf(c.Prediction.Tier), DeadlineMinutes: DefaultDeadlineMinutes}
	var pct *float64
	if c.Prediction.SizePct != nil {
		v := min(math.Abs(*c.Prediction.SizePct), maxExpectedPct)
		pct = &v
	}
	if pct != nil && *pct > 0 && s.ExpectedSource != changeplan.SourceNone &&
		s.ExpectedDirection != changeplan.DirectionNone && s.Metric != changeplan.MetricReadback {
		s.ExpectedPct = pct
	}
	info := Step{Origin: OriginGenerator, OriginRef: c.Generator, CandidateID: c.ID,
		Kind: c.Kind, Target: c.Target, Title: c.Reason,
		EvidenceIDs: append([]string(nil), c.EvidenceIDs...),
		Prediction: Prediction{Tier: c.Prediction.Tier, ImprovementPct: pct,
			Metric: c.Prediction.Metric, Direction: c.Prediction.Direction}}
	if c.Approval != candidate.ApprovalAutomatic {
		info.ApprovalRequired, info.ApprovalReason = true, c.ApprovalReason
	}
	return proposal{step: s, info: info, tables: r.tables}
}

func appendNew(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}
