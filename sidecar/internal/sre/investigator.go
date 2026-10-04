package sre

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// The tool-calling investigator (roadmap 2.1). After the deterministic
// probe plan and diagnosis, the model plans further reads over the
// catalog through the bounded agent loop (internal/agentloop), then
// agrees, concludes an inconclusive graph, contests a conclusive root or
// names an unmodeled cause, citing evidence. Its conclusion is applied
// under the authority rule (investigator_outcome.go); the deterministic
// diagnosis stands whenever the model stops without a usable answer.
// Nothing the model says executes: remediation stays a typed proposal
// through policy.Gate, outside this package.

// StageInvestigator names the investigator in model_rejected events.
const StageInvestigator = "investigator"

// investigatorSession is one tool-calling investigation run.
type investigatorSession struct {
	c         *Coordinator
	plan      InvestigatorPlan
	lease     Lease
	inv       Investigation
	calls     int // model calls (reservation keys)
	commits   int // reads committed as evidence
	memory    string
	memoryRef *MemoryRef
}

// investigate runs the investigator on a diagnosis. It returns the
// current lease, the diagnosis to conclude and what is stored beside it.
func (c *Coordinator) investigate(ctx context.Context, lease Lease, inv Investigation,
	d causal.Diagnosis, ev []Evidence) (Lease, causal.Diagnosis, modelOutcome, error) {
	s := &investigatorSession{c: c, lease: lease, inv: inv,
		plan: c.investigator.plans()[PlanForTrigger(inv.TriggerKind)]}
	ms := &modelSession{c: c, lease: lease, inv: inv}
	ms.recall(ctx, d)
	s.memory, s.memoryRef = ms.memory, ms.memoryRef
	none := modelOutcome{memory: s.memoryRef}
	cfg, rej, err := s.loopConfig(ctx, d, ev)
	if err != nil || rej != nil {
		return s.lease, d, none, errors.Join(err, s.rejected(ctx, rej))
	}
	res, err := agentloop.Run(ctx, investigatorModel{s: s}, cfg)
	if err != nil {
		return s.lease, d, none, err
	}
	return s.finish(ctx, res)
}

// loopConfig is the agent loop's run under the plan, within the active
// time and the investigation's probe ceiling.
func (s *investigatorSession) loopConfig(ctx context.Context, d causal.Diagnosis,
	ev []Evidence) (agentloop.Config, *ModelRejection, error) {
	cur, err := s.c.store.Get(ctx, s.lease.Scope, s.inv.ID)
	if err != nil {
		return agentloop.Config{}, nil, err
	}
	wall := min(s.plan.Wall, time.Until(s.lease.SegmentDeadline)-stepMargin)
	if wall < minModelTime {
		return agentloop.Config{}, reject(RejectNoTime, "%s of active time left",
			time.Until(s.lease.SegmentDeadline).Round(time.Second)), nil
	}
	tools := s.tools()
	return agentloop.Config{System: investigatorRules,
		Task: investigatorTask(s.inv, d, s.memory, s.c.promptFacts(ctx),
			toolNames(tools)),
		Tools: tools, Final: investigatorFinal(), Seed: seedEvidence(d, ev),
		Protocol: s.c.investigator.protocol(), Ground: groundClaim, MaxClaims: MaxClaims,
		Budget: agentloop.Budget{MaxSteps: s.plan.MaxSteps, MaxCalls: 3 * s.plan.MaxSteps,
			MaxCost:   s.plan.probeBudget(s.c.store.Limits(), cur.ProbeCount),
			Wall:      wall,
			MaxTokens: int(s.plan.MaxTokens), StepTokens: s.plan.StepTokens,
			StepTimeout: s.c.cfg.ModelTimeout}}, nil, nil
}

// seedEvidence is the stored evidence the model may cite from its first
// step, with the diagnosis' facts as its text; evidence whose hash no
// longer verifies is not citable.
func seedEvidence(d causal.Diagnosis, ev []Evidence) []agentloop.Evidence {
	cat := buildEvidenceCatalog(d, ev)
	out := make([]agentloop.Evidence, 0, len(cat))
	for i, e := range cat {
		if e.stale {
			continue
		}
		out = append(out, agentloop.Evidence{ID: string(e.id),
			Digest: hex.EncodeToString(ev[i].SHA256), Label: e.probe + " " + e.status,
			Text: e.text})
	}
	return out
}

// finish re-diagnoses on all evidence and applies the model's answer
// under the authority rule.
func (s *investigatorSession) finish(ctx context.Context, res agentloop.Result) (Lease,
	causal.Diagnosis, modelOutcome, error) {
	d, stored, err := s.diagnosis(ctx)
	if err != nil {
		return s.lease, d, modelOutcome{memory: s.memoryRef}, err
	}
	run := s.transcript(res)
	out := modelOutcome{memory: s.memoryRef, run: run}
	if res.Final == nil {
		return s.lease, d, out, s.noConclusion(ctx, run)
	}
	narrative := verifiedNarrative(res.Claims, stored)
	kept := 0
	if narrative != nil {
		kept = len(narrative.Claims)
	}
	r := inconclusive(DowngradeInvalidOutcome)
	if f, ferr := decodeFinal(res.Final); ferr == nil {
		r = resolveOutcome(d, f, kept)
	}
	var g RootGrant
	if r.outcome == ModelContested || r.outcome == ModelConcluded {
		g = s.c.rootAuthority(ctx, s.inv, authorityFamily(d, r.root))
	}
	concluded, mc, contest := applyAuthority(d, r, g)
	out.conclusion, out.contest, out.narrative = mc, contest, narrative
	if mc.Authority == ContestAdopted {
		graph := d
		out.graph = &graph
	}
	return s.lease, concluded, out, s.recordOutcome(ctx, mc, run)
}

// verifiedNarrative keeps the claims whose every cited evidence is still
// stored and matches its hash; claim text is redacted like all stored
// text.
func verifiedNarrative(claims []agentloop.Claim, stored []Evidence) *Narrative {
	ok := map[UUID]bool{}
	for _, e := range stored {
		ok[e.ID] = e.VerifyHash()
	}
	n := &Narrative{Label: NarrativeLabel}
	for _, c := range claims {
		claim := NarrativeClaim{Text: truncateRunes(RedactText(c.Text), MaxClaimRunes)}
		good := len(c.EvidenceIDs) > 0 && len(c.EvidenceIDs) <= maxFacts
		for _, id := range c.EvidenceIDs {
			good = good && ok[UUID(id)]
			claim.EvidenceIDs = append(claim.EvidenceIDs, UUID(id))
		}
		if good && len(n.Claims) < MaxClaims {
			n.Claims = append(n.Claims, claim)
		}
	}
	if len(n.Claims) == 0 {
		return nil
	}
	return n
}

// transcript is the stored form of the loop's transcript.
func (s *investigatorSession) transcript(res agentloop.Result) *InvestigatorRun {
	t := res.Transcript
	run := &InvestigatorRun{Label: InvestigatorRunLabel, Plan: s.plan.Name,
		Budget: InvestigatorBudget{MaxSteps: s.plan.MaxSteps, MaxProbes: s.plan.MaxProbes,
			WallMS: s.plan.Wall.Milliseconds(), MaxTokens: s.plan.MaxTokens},
		Protocol:   string(t.Protocol),
		ModelPlan:  cleanLine(t.Plan, maxModelPlanRunes),
		ModelCalls: t.ModelCalls, ToolCalls: t.ToolCalls, Probes: t.Cost, Tokens: t.Tokens,
		Rejected: t.Rejected, Stop: t.Stop,
		StopDetail: cleanLine(t.StopDetail, maxStepNoteRunes)}
	if len(run.Rejected) == 0 {
		run.Rejected = nil
	}
	for _, d := range res.Dropped {
		if run.DroppedClaims == nil {
			run.DroppedClaims = map[string]int{}
		}
		run.DroppedClaims[d.Reason]++
	}
	steps := t.Steps
	if len(steps) > MaxInvestigatorSteps {
		steps = append(append([]agentloop.Step(nil), steps[:MaxInvestigatorSteps-1]...),
			steps[len(steps)-1])
	}
	for _, st := range steps {
		run.Steps = append(run.Steps, storedStep(st))
	}
	return run
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// cleanLine is model or provider text as one stored line: secrets and
// PII-like literals removed, whitespace (control characters included)
// collapsed, bounded.
func cleanLine(s string, n int) string {
	return truncateRunes(strings.Join(strings.Fields(scrub(s)), " "), n)
}

// storedStep bounds and redacts one transcript step; a tool name the
// model made up that is no tool name is replaced.
func storedStep(st agentloop.Step) InvestigatorStep {
	tool := st.Tool
	if tool != "" && !toolNamePattern.MatchString(tool) {
		tool = "invalid_tool_name"
	}
	out := InvestigatorStep{Seq: st.Seq, Call: st.ModelCall, Tool: tool,
		Status: truncateRunes(st.Status, 64), EvidenceID: UUID(st.EvidenceID),
		Digest: st.Digest, Cost: st.Cost, ElapsedMS: st.ElapsedMS,
		Note: cleanLine(st.Note, maxStepNoteRunes)}
	if raw, err := redactJSON(st.Args); err == nil && len(raw) <= maxStepArgsBytes &&
		json.Valid(raw) {
		out.Args = raw
	}
	return out
}
