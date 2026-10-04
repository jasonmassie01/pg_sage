package sre

import "context"

// Investigator events on the investigation's hash chain: the accepted
// outcome (model_reviewed, with the plan, counts and stop reason), a
// contest of the graph's root (model_disagreed, read by the bench and the
// replay export), and a run that ended without a usable answer
// (model_rejected, with its stop reason). A disabled client is said once
// per process instead.

// rejected records a run that could not start.
func (s *investigatorSession) rejected(ctx context.Context, rej *ModelRejection) error {
	if rej == nil {
		return nil
	}
	s.c.logFn("INFO", "sre: investigation %s: the investigator did not run (%s): %s",
		s.inv.ID, rej.Reason, rej.Detail)
	return s.c.store.RecordEvent(ctx, s.lease, EventModelRejected, map[string]any{
		"reason": rej.Reason, "detail": rej.Detail, "stage": StageInvestigator,
		"investigator": s.plan.Name})
}

// noConclusion records a run that stopped without a final answer; the
// deterministic diagnosis stands.
func (s *investigatorSession) noConclusion(ctx context.Context, run *InvestigatorRun) error {
	if run.Stop == "llm_disabled" {
		NoteModelUnavailable(s.c.notices, s.c.logFn, run.StopDetail)
		return nil
	}
	s.c.logFn("INFO", "sre: investigation %s: the investigator stopped without a "+
		"conclusion (%s): %s", s.inv.ID, run.Stop, run.StopDetail)
	return s.c.store.RecordEvent(ctx, s.lease, EventModelRejected, map[string]any{
		"reason": run.Stop, "detail": run.StopDetail, "stage": StageInvestigator,
		"investigator": run.Plan, "model_calls": run.ModelCalls, "probes": run.Probes})
}

// recordOutcome records the accepted outcome and, for a contest, the
// disagreement with the graph's root.
func (s *investigatorSession) recordOutcome(ctx context.Context, mc *ModelConclusion,
	run *InvestigatorRun) error {
	dropped, rejected := 0, 0
	for _, n := range run.DroppedClaims {
		dropped += n
	}
	for _, n := range run.Rejected {
		rejected += n
	}
	if err := s.c.store.RecordEvent(ctx, s.lease, EventModelReviewed, map[string]any{
		"investigator": run.Plan, "outcome": mc.Outcome, "model_root": mc.Root,
		"authority": mc.Authority, "model_calls": run.ModelCalls,
		"tool_calls": run.ToolCalls, "probes": run.Probes, "stop": run.Stop,
		"dropped_claims": dropped, "rejected_calls": rejected}); err != nil {
		return err
	}
	if mc.Outcome != ModelContested {
		return nil
	}
	s.c.logFn("INFO", "sre: investigation %s: the investigator contests the graph's root "+
		"%s with %s (%s): %s", s.inv.ID, mc.GraphRoot, mc.Root, mc.Authority, mc.Reason)
	return s.c.store.RecordEvent(ctx, s.lease, EventModelDisagreed, map[string]any{
		"graph_root": mc.GraphRoot, "model_root": mc.Root, "authority": mc.Authority,
		"reason": mc.Reason, "investigator": run.Plan, "turns": run.ModelCalls})
}
