package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/modellift"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// maxExpectedPct is plan-01's bound on a step's expected change.
const maxExpectedPct = 1000.0

// modelSteps turns each surviving hypothesis into a proposal or advice.
// Only a cancelled context is an error.
func (b *builder) modelSteps(ctx context.Context) error {
	for _, h := range b.in.Report.Hypotheses {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("build the review plan of run %d: %w", b.in.RunID, err)
		}
		p, err := b.hypothesis(ctx, h)
		if err == nil {
			b.props = append(b.props, p)
			continue
		}
		var r *refusal
		if !errors.As(err, &r) {
			return err
		}
		b.advise(Advice{Origin: OriginModel, OriginRef: b.modelRef(),
			Kind: strings.TrimSpace(h.Kind), Target: h.Target, Title: h.Title,
			Reason: r.reason, Detail: r.detail, EvidenceIDs: claimIDs(h)})
	}
	return nil
}

// hypothesis is h as a step, or the refusal that keeps it advice: no
// template, no contract, no evidence that resolves, a target that does not
// resolve, SQL the executor refuses, nothing to measure, or a ladder that
// cannot test it or predicts too little.
func (b *builder) hypothesis(ctx context.Context, h report.Hypothesis) (proposal, error) {
	kind := strings.ToLower(strings.TrimSpace(h.Kind))
	tpl, ok := templates[kind]
	if !ok {
		return proposal{}, refuse(ReasonNoTemplate, "no template for kind %q: a model "+
			"hypothesis becomes a step only through a typed template (%s)", h.Kind,
			templateKinds())
	}
	if _, ok := executor.ContractForActionType(tpl.action); !ok {
		return proposal{}, refuse(ReasonNoContract, "no executor contract for action type "+
			"%s yet, so %s cannot run", tpl.action, kind)
	}
	evidence := b.lp.resolves(claimIDs(h))
	if len(evidence) == 0 {
		return proposal{}, refuse(ReasonNoEvidence, "the hypothesis cites no evidence id "+
			"that resolves in the pack")
	}
	t, err := parseTarget(h.Target, b.in.Pseudonyms)
	if err != nil {
		return proposal{}, err
	}
	r, err := tpl.render(b, t)
	if err != nil {
		return proposal{}, err
	}
	if err := checkSQL(r); err != nil {
		return proposal{}, err
	}
	if err := measurable(r); err != nil {
		return proposal{}, err
	}
	r.proposal.Kind, r.proposal.ActionType = kind, tpl.action
	m, err := b.predict(ctx, r.proposal)
	if err != nil {
		return proposal{}, err
	}
	return b.modelProposal(h, kind, tpl.action, r, evidence, m), nil
}

func checkSQL(r rendered) error {
	for _, which := range []string{"forward", "rollback"} {
		sql := r.sql
		if which == "rollback" {
			sql = r.rollback
		}
		if sql == "" && which == "rollback" {
			continue // an irreversible contract has none; plan-01 checks the pairing
		}
		if err := executor.ValidateExecutorSQL(sql); err != nil {
			return refuse(ReasonSQLRefused, "the executor refuses the rendered %s SQL %q: %v",
				which, sql, err)
		}
	}
	return nil
}

// measurable refuses a step whose verdict could not be judged: no target
// statement, or targets that ran fewer than the calls a verdict needs.
func measurable(r rendered) error {
	if r.metric == changeplan.MetricReadback {
		return nil
	}
	if len(r.targets) == 0 {
		return refuse(ReasonNotMeasurable, "no statement of the window touches %s on these "+
			"columns, so no verdict could measure the change", strings.Join(r.tables, ", "))
	}
	if n := calls(r.targets); n < changeplan.DefaultMinCalls {
		return refuse(ReasonNotMeasurable, "its statements ran %d times in the window, "+
			"fewer than the %d a verdict needs", n, changeplan.DefaultMinCalls)
	}
	return nil
}

// predict runs the ladder; only a prediction of at least the minimum
// improvement passes.
func (b *builder) predict(ctx context.Context, p Proposal) (Measured, error) {
	if b.in.Ladder == nil {
		return Measured{}, refuse(ReasonNoLadderTier, "no ladder tier is available to test "+
			"%s, so a model hypothesis stays advice", p.Kind)
	}
	m, err := b.in.Ladder.Predict(ctx, p)
	switch {
	case errors.Is(err, ErrNoTier):
		return Measured{}, refuse(ReasonNoLadderTier, "%v", err)
	case err != nil:
		return Measured{}, refuse(ReasonLadderError, "the ladder could not test %s: %v",
			p.Kind, err)
	case m.ImprovementPct <= 0 || m.ImprovementPct < b.in.MinImprovementPct:
		return Measured{}, refuse(ReasonBelowMinimum, "the %s tier predicts %.2f%%, below "+
			"the minimum improvement of %.2f%%", m.Tier, m.ImprovementPct,
			b.in.MinImprovementPct)
	}
	return m, nil
}

func (b *builder) modelProposal(h report.Hypothesis, kind, action string, r rendered,
	evidence []string, m Measured) proposal {
	pct := min(m.ImprovementPct, maxExpectedPct)
	s := changeplan.Step{Kind: changeplan.KindAction, ActionType: action,
		Setting: r.setting, ProposedSQL: r.sql, RollbackSQL: r.rollback,
		EvidenceIDs: evidence, TargetQueryIDs: ids(r.targets), Metric: r.metric,
		ExpectedDirection: r.direction, ExpectedSource: sourceOf(m.Tier),
		DeadlineMinutes: DefaultDeadlineMinutes}
	if r.metric != changeplan.MetricReadback && s.ExpectedSource != changeplan.SourceNone {
		s.ExpectedPct = &pct
	}
	info := Step{Origin: OriginModel, OriginRef: b.modelRef(), Kind: kind, Target: h.Target,
		Title: h.Title, EvidenceIDs: evidence,
		Prediction: Prediction{Tier: m.Tier, ImprovementPct: &pct, Metric: string(r.metric),
			Direction: string(r.direction)}}
	return proposal{step: s, info: info, tables: r.tables}
}

// modelRef is the normalized model id ("unknown" when none was named).
func (b *builder) modelRef() string {
	if m := modellift.NormalizeModel(b.in.Model); m != "" {
		return m
	}
	return "unknown"
}

// claimIDs are the durable evidence ids h's claims cite, once each.
func claimIDs(h report.Hypothesis) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range h.Claims {
		for _, id := range c.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}
