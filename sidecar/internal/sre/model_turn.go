package sre

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// The model turn (Sage SRE M3). After the deterministic diagnosis, an
// optional model reviews it: it may reorder the graph's open hypotheses,
// ask for one catalog probe while the graph is inconclusive, and narrate
// cited claims. Every reply is validated, gets one repair turn, and on
// failure the investigation concludes with the deterministic result and
// a model_rejected event. Budgets (AI-SRE-SPEC §11): at most the store's
// model turns (2), its input/output tokens split evenly per turn, and
// the investigation's remaining active time. The model never fails or
// blocks an investigation; only a lost lease or an unavailable store
// stops the run, as it would without the model.

// Model turn stages, recorded with rejections.
const (
	stageReview  = "review"
	stageFinal   = "final_review"
	stageProbe   = "next_probe"
	stageVerify  = "verifier"
	minModelTime = time.Second
)

// modelOutcome is what the model adds to a conclusion's summary.
type modelOutcome struct {
	ranking   *ModelRanking
	narrative *Narrative
	probe     *ModelProbe
	// memory is what the turn was offered as context; it is not model
	// output and is kept whatever the model replied.
	memory *MemoryRef
}

func (o modelOutcome) empty() bool {
	return o.ranking == nil && o.narrative == nil && o.probe == nil
}

func (o modelOutcome) apply(s *Summary) {
	s.ModelRanking, s.Narrative, s.ModelProbe = o.ranking, o.narrative, o.probe
	s.Memory = o.memory
}

// modelSession is one investigation run's use of the model.
type modelSession struct {
	c        *Coordinator
	lease    Lease
	inv      Investigation
	turns    int  // turns attempted in this run
	tools    bool // false after the provider refused tool calling
	repaired string
	// memory is the fenced past-incident context of every turn and
	// memoryRef the investigations it shows.
	memory    string
	memoryRef *MemoryRef
}

// consultModel runs the model turn on a diagnosis. It returns the
// current lease, the (possibly re-run) diagnosis and the model output to
// store beside it.
func (c *Coordinator) consultModel(ctx context.Context, lease Lease, inv Investigation,
	d causal.Diagnosis, ev []Evidence) (Lease, causal.Diagnosis, modelOutcome, error) {
	if c.model == nil {
		return lease, d, modelOutcome{}, nil
	}
	if !c.model.IsEnabled() {
		NoteModelUnavailable(c.notices, c.logFn, "the LLM client is disabled")
		return lease, d, modelOutcome{}, nil
	}
	s := &modelSession{c: c, lease: lease, inv: inv, tools: true}
	s.recall(ctx, d)
	out := modelOutcome{memory: s.memoryRef}
	review, rej, err := s.ask(ctx, newReviewScope(d, ev, !d.Conclusive))
	if err != nil || rej != nil {
		return s.lease, d, out, errors.Join(err, s.rejected(ctx, rej, stageReview))
	}
	if review.NextProbe != nil {
		d, review, out.probe, err = s.followProbe(ctx, d, review)
		if err != nil {
			return s.lease, d, modelOutcome{memory: s.memoryRef}, err
		}
	}
	out, err = s.finish(ctx, d, review, out)
	return s.lease, d, out, err
}

// ask runs one turn and, when its reply is rejected for a repairable
// reason and a turn is left, one repair turn.
func (s *modelSession) ask(ctx context.Context, scope reviewScope) (modelReview,
	*ModelRejection, error) {
	scope.memory = s.memory
	scope.facts = s.c.promptFacts(ctx)
	r, rej, err := s.attempt(ctx, scope, "")
	if err != nil || rej == nil || !repairable[rej.Reason] || s.turnsLeft() == 0 {
		return r, rej, err
	}
	r, again, err := s.attempt(ctx, scope, rej.Reason+": "+rej.Detail)
	if err == nil && again == nil {
		s.repaired = rej.Reason
	}
	return r, again, err
}

func (s *modelSession) turnsLeft() int {
	return max(0, s.c.store.Limits().MaxModelTurns-s.inv.ModelTurns-s.turns)
}

// perTurn splits the investigation's token caps evenly over its turns:
// input, answer and, for a thinking model only, reasoning.
func (s *modelSession) perTurn() (int64, int64, int64) {
	l := s.c.store.Limits()
	turns := int64(max(1, l.MaxModelTurns))
	var reasoning int64
	if s.c.model.ThinkingModel() {
		reasoning = l.MaxReasoningTokens / turns
	}
	return l.MaxInputTokens / turns, l.MaxOutputTokens / turns, reasoning
}

// timeout is the turn's time: the configured cap within the active time
// left before the conclusion's margin.
func (s *modelSession) timeout() (time.Duration, bool) {
	left := time.Until(s.lease.SegmentDeadline) - stepMargin
	if left < minModelTime {
		return 0, false
	}
	return min(s.c.cfg.ModelTimeout, left), true
}

// attempt makes one model call and checks its reply.
func (s *modelSession) attempt(ctx context.Context, scope reviewScope,
	repair string) (modelReview, *ModelRejection, error) {
	timeout, ok := s.timeout()
	if !ok {
		return modelReview{}, reject(RejectNoTime, "%s of active time left",
			time.Until(s.lease.SegmentDeadline).Round(time.Second)), nil
	}
	msgs := reviewMessages(s.inv, scope, repair, s.tools)
	var tools []llm.ToolSpec
	if s.tools {
		tools = reviewToolsFor(scope.allowProbe)
	}
	in, out, reasoning := s.perTurn()
	// About 4 bytes a token, with a quarter left for JSON escaping.
	if n := promptBytes(msgs, tools); int64(n) > in*3 {
		return modelReview{}, reject(RejectOversizedPrompt, "prompt of %d bytes over "+
			"the %d-token turn reservation", n, in), nil
	}
	s.turns++
	res, err := s.call(ctx, ModelTurn{Messages: msgs, Tools: tools,
		Options: llm.ToolOptions{Timeout: timeout}, Input: in, Output: out,
		Reasoning:  reasoning,
		RequestKey: fmt.Sprintf("model-f%d-%d", s.lease.Fence, s.inv.ModelTurns+s.turns)})
	if err != nil {
		return modelReview{}, nil, err
	}
	if res.rejection != nil {
		return modelReview{}, res.rejection, nil
	}
	parsed, rej := parseModelReply(res.result)
	if rej != nil {
		return modelReview{}, rej, nil
	}
	review, rej := scope.check(parsed)
	return review, rej, nil
}

func promptBytes(msgs []llm.Message, tools []llm.ToolSpec) int {
	n := 1024 // request envelope
	for _, m := range msgs {
		n += len(m.Content) + 32
	}
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters) + 64
	}
	return n
}
