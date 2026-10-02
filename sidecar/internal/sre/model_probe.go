package sre

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// followProbe runs the model's one next probe (inconclusive graphs
// only), re-runs the deterministic diagnosis on all stored evidence and,
// with a turn left, has the model review the new diagnosis. A failed
// final review keeps the first one; the verifier then decides what of it
// still holds.
func (s *modelSession) followProbe(ctx context.Context, d causal.Diagnosis,
	review modelReview) (causal.Diagnosis, modelReview, *ModelProbe, error) {
	probe, key, rej, err := s.runProbe(ctx, *review.NextProbe)
	if err != nil || rej != nil {
		return d, review, nil, errors.Join(err, s.rejected(ctx, rej, stageProbe))
	}
	ev, err := s.c.store.Evidence(ctx, s.lease.Scope, s.inv.ID)
	if err != nil {
		return d, review, nil, err
	}
	for _, e := range ev {
		if e.StepKey == key {
			probe.EvidenceID = e.ID
		}
	}
	obs, err := observations(ev)
	if err != nil {
		return d, review, nil, err
	}
	d = diagnose(s.inv, obs)
	if s.turnsLeft() == 0 {
		return d, review, probe, nil
	}
	final, rej, err := s.ask(ctx, newReviewScope(d, ev, false))
	if err != nil || rej != nil {
		return d, review, probe, errors.Join(err, s.rejected(ctx, rej, stageFinal))
	}
	return d, final, probe, nil
}

// runProbe runs and commits the proposed probe within the probe ceiling
// and the active time, moving evaluating -> needs_evidence (with the
// evidence) -> collecting -> evaluating.
func (s *modelSession) runProbe(ctx context.Context, p ProposedProbe) (*ModelProbe,
	string, *ModelRejection, error) {
	cur, err := s.c.store.Get(ctx, s.lease.Scope, s.inv.ID)
	if err != nil {
		return nil, "", nil, err
	}
	if ceiling := s.c.store.Limits().MaxProbes; cur.ProbeCount+1 > ceiling {
		return nil, "", reject(RejectProbeBudget, "%d probes ran; the ceiling is %d",
			cur.ProbeCount, ceiling), nil
	}
	if time.Until(s.lease.SegmentDeadline) < 2*stepMargin {
		return nil, "", reject(RejectNoTime, "no active time left for %s", p.ID), nil
	}
	if s.lease, err = s.c.store.Heartbeat(ctx, s.lease); err != nil {
		return nil, "", nil, err
	}
	res := s.c.runner.Run(ctx, p.ID, p.Args)
	key := fmt.Sprintf("model-probe-f%d", s.lease.Fence)
	_, err = s.c.store.CommitStep(ctx, s.lease, StepResult{IdempotencyKey: key,
		Results: []probes.Result{res}, NextState: StateNeedsEvidence})
	if errors.Is(err, ErrBudgetExhausted) {
		return nil, "", reject(RejectProbeBudget, "%v", err), nil
	}
	if err != nil {
		return nil, "", nil, err
	}
	for _, step := range []struct {
		key  string
		next State
	}{{"model-collect", StateCollecting}, {"model-evaluate", StateEvaluating}} {
		if _, err := s.c.store.CommitStep(ctx, s.lease, StepResult{NextState: step.next,
			IdempotencyKey: fmt.Sprintf("%s-f%d", step.key, s.lease.Fence)}); err != nil {
			return nil, "", nil, err
		}
	}
	return modelProbeOf(p), key, nil, nil
}

func modelProbeOf(p ProposedProbe) *ModelProbe {
	out := &ModelProbe{Label: ModelProbeLabel, ProbeID: string(p.ID), PID: p.Args.PID,
		WindowSeconds: int64(p.Args.Window / time.Second), Rationale: p.Rationale}
	if !p.Args.BackendStart.IsZero() {
		start := p.Args.BackendStart
		out.BackendStart = &start
	}
	return out
}

// finish runs the verifier pass on the accepted review, records what was
// rejected, a disagreement or the accepted review, and returns what is
// stored beside the deterministic diagnosis.
func (s *modelSession) finish(ctx context.Context, d causal.Diagnosis, review modelReview,
	out modelOutcome) (modelOutcome, error) {
	stored, err := s.c.store.Evidence(ctx, s.lease.Scope, s.inv.ID)
	if err != nil {
		return modelOutcome{}, err
	}
	v := verifyReview(review, d, stored)
	if v.disagreed {
		s.c.logFn("INFO", "sre: investigation %s: the model ranks %s first; the "+
			"graph's root cause %s stands", s.inv.ID, v.modelRoot, v.graphRoot)
		return modelOutcome{probe: out.probe}, s.c.store.RecordEvent(ctx, s.lease,
			EventModelDisagreed, map[string]any{"graph_root": v.graphRoot,
				"model_root": v.modelRoot, "turns": s.inv.ModelTurns + s.turns})
	}
	for _, rej := range v.rejected {
		if err := s.rejected(ctx, rej, stageVerify); err != nil {
			return modelOutcome{}, err
		}
	}
	out.ranking, out.narrative = rankingOf(v.review), narrativeOf(v.review)
	if out.empty() {
		return out, nil
	}
	return out, s.c.store.RecordEvent(ctx, s.lease, EventModelReviewed, s.reviewed(out))
}

func (s *modelSession) reviewed(out modelOutcome) map[string]any {
	p := map[string]any{"turns": s.inv.ModelTurns + s.turns, "repaired": s.repaired,
		"ranking": []string{}, "claims": 0, "next_probe": ""}
	if out.ranking != nil {
		p["ranking"] = out.ranking.Nodes
	}
	if out.narrative != nil {
		p["claims"] = len(out.narrative.Claims)
	}
	if out.probe != nil {
		p["next_probe"] = out.probe.ProbeID
	}
	return p
}

func rankingOf(r modelReview) *ModelRanking {
	if len(r.Ranking) == 0 {
		return nil
	}
	return &ModelRanking{Label: ModelRankingLabel, Basis: ModelRankingBasis,
		Nodes: append([]string(nil), r.Ranking...)}
}

// narrativeOf binds the verified claims to stored evidence ids; claim
// text is redacted like every other stored text.
func narrativeOf(r modelReview) *Narrative {
	if len(r.Claims) == 0 {
		return nil
	}
	n := &Narrative{Label: NarrativeLabel}
	for _, c := range r.Claims {
		claim := NarrativeClaim{Text: truncateRunes(RedactText(c.Text), MaxClaimRunes)}
		for _, alias := range c.EvidenceIDs {
			claim.EvidenceIDs = append(claim.EvidenceIDs, r.aliases[alias])
		}
		n.Claims = append(n.Claims, claim)
	}
	return n
}
