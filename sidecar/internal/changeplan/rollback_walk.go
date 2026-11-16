package changeplan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The revert_to_checkpoint walk (plan-02c), a pure function of the plan's
// rows so a restarted engine resumes it where it stopped. The walk covers
// the steps after the latest checkpoint before the failed step (by
// ordinal; the plan's start when there is none) up to the failed step,
// and reverts its kept steps in reverse run order. A step a kept step
// outside the walk depends on is not reverted: the walk stops there. A
// step whose action needs no rollback (ANALYZE, VACUUM) stays kept with
// no_rollback_needed; any other class stops the walk. The plan then halts
// with what was reverted and what was kept, and why.

// ReasonNoRollbackNeeded is a kept step's reason when the walk passed it
// because its action has nothing to undo.
const ReasonNoRollbackNeeded = "no_rollback_needed"

// keptStep is a step in the walk that the walk did not revert, and why.
type keptStep struct {
	Ordinal int    `json:"ordinal"`
	Why     string `json:"why"`
}

// walk is where a revert_to_checkpoint walk stands.
type walk struct {
	origin     Step
	checkpoint int        // the checkpoint step's ordinal; 0 is the plan's start
	next       *Step      // the next kept step to revert; nil once the walk ended
	reverted   []int      // the walk's reverted steps, in walk order
	kept       []keptStep // the walk's kept steps, in walk order, with why
	stopped    string     // why the walk stopped short of the checkpoint
}

// walkOrigin is the step whose verdict started a walk: reverted (by the
// plan or the verifier) or skipped as expired, with revert_to_checkpoint
// as its verdict's action.
func walkOrigin(p Plan) (Step, bool) {
	for _, s := range p.Steps {
		started := s.Runtime.State == StateReverted ||
			(s.Runtime.State == StateSkipped && s.Runtime.Verdict == OutcomeExpired)
		if started && s.Runtime.Verdict != "" &&
			s.ActionFor(s.Runtime.Verdict) == ActionRevertToCheckpoint {
			return s, true
		}
	}
	return Step{}, false
}

// walkMove is the walk's next move: revert its next step, or halt with
// its summary once it ended. None while no walk runs, or once a step
// failed or halted (the plan halts on that step instead).
func walkMove(p Plan) (move, bool) {
	for _, s := range p.Steps {
		if s.Runtime.State == StateFailed || s.Runtime.State == StateHalted {
			return move{}, false
		}
	}
	origin, ok := walkOrigin(p)
	if !ok {
		return move{}, false
	}
	w := planWalk(p, origin)
	if w.next != nil {
		return move{kind: moveWalkRevert, step: *w.next, walk: &w}, true
	}
	return move{kind: moveHalt, step: origin, reason: w.summary(), walk: &w}, true
}

// checkpointBefore is the ordinal of the latest checkpoint step before
// ordinal (0, the plan's start, when there is none).
func checkpointBefore(p Plan, ordinal int) int {
	cp := 0
	for _, s := range p.Steps {
		if s.Checkpoint && s.Ordinal < ordinal && s.Ordinal > cp {
			cp = s.Ordinal
		}
	}
	return cp
}

// planWalk is the walk back from origin as p's rows stand.
func planWalk(p Plan, origin Step) walk {
	w := walk{origin: origin, checkpoint: checkpointBefore(p, origin.Ordinal)}
	inWalk := map[int]bool{}
	byOrdinal := map[int]Step{}
	for _, s := range p.Steps {
		byOrdinal[s.Ordinal] = s
		inWalk[s.Ordinal] = s.Ordinal > w.checkpoint && s.Ordinal <= origin.Ordinal
	}
	for _, ordinal := range reverseTopo(p) {
		s := byOrdinal[ordinal]
		if !inWalk[ordinal] {
			continue
		}
		switch {
		case s.Runtime.State == StateReverted:
			w.reverted = append(w.reverted, ordinal)
		case s.Runtime.State != StateKept:
			// Never ran, or ended without a change: nothing to revert.
		case w.stopped != "":
			w.kept = append(w.kept, keptStep{ordinal, "the walk stopped at a later step"})
		default:
			if w.consider(p, s, inWalk) {
				return w
			}
		}
	}
	return w
}

// consider decides kept step s: true when it is the walk's next revert.
func (w *walk) consider(p Plan, s Step, inWalk map[int]bool) bool {
	if d, ok := keptDependantOutside(p, s.Ordinal, inWalk); ok {
		w.stop(s, fmt.Sprintf("kept step %d depends on it and is outside the walk", d))
		return false
	}
	class, why := walkClass(s)
	switch {
	case why != "":
		w.stop(s, why)
	case class == policy.RollbackReversible && s.Runtime.ActionLogID == nil:
		w.stop(s, "it has no action to revert")
	case class == policy.RollbackReversible:
		w.next = &s
		return true
	case class == policy.RollbackNoRollbackNeeded:
		if d, ok := revertedDependant(p, s.Ordinal); ok {
			w.stop(s, fmt.Sprintf("%s, but reverted step %d depends on it",
				ReasonNoRollbackNeeded, d))
			return false
		}
		w.kept = append(w.kept, keptStep{s.Ordinal, ReasonNoRollbackNeeded})
	default:
		w.stop(s, fmt.Sprintf("%s is %s: pg_sage does not revert it", s.ActionType, class))
	}
	return false
}

func (w *walk) stop(s Step, why string) {
	w.stopped = fmt.Sprintf("step %d: %s", s.Ordinal, why)
	w.kept = append(w.kept, keptStep{s.Ordinal, why})
}

// walkClass is how kept step s undoes: the rollback class of the executor
// contracts it ran under (its action type and the one its SQL maps to).
// A check step changed nothing. why is set for a step pg_sage cannot
// classify or revert (an operator's step, an action without a contract).
func walkClass(s Step) (policy.RollbackClass, string) {
	switch s.Kind {
	case KindCheck:
		return policy.RollbackNoRollbackNeeded, ""
	case KindOperator:
		return "", "an operator applied it: pg_sage does not revert an operator step"
	}
	class := policy.RollbackNoRollbackNeeded
	for _, actionType := range []string{s.ActionType, executor.ActionTypeForSQL(s.ProposedSQL)} {
		c, ok := executor.ContractForActionType(actionType)
		if !ok {
			return "", fmt.Sprintf("the executor has no contract for action type %q, so "+
				"pg_sage cannot tell how to revert it", actionType)
		}
		switch got := policy.RollbackClass(c.RollbackClass); got {
		case policy.RollbackNoRollbackNeeded:
		case policy.RollbackReversible:
			class = got
		default:
			return got, ""
		}
	}
	return class, ""
}

// keptDependantOutside is a kept step outside the walk that depends on
// step ordinal, directly or through other steps.
func keptDependantOutside(p Plan, ordinal int, inWalk map[int]bool) (int, bool) {
	state := map[int]StepState{}
	for _, s := range p.Steps {
		state[s.Ordinal] = s.Runtime.State
	}
	for _, d := range descendants(p, ordinal) {
		if state[d] == StateKept && !inWalk[d] {
			return d, true
		}
	}
	return 0, false
}

// revertedDependant is a reverted step that depends on step ordinal.
func revertedDependant(p Plan, ordinal int) (int, bool) {
	state := map[int]StepState{}
	for _, s := range p.Steps {
		state[s.Ordinal] = s.Runtime.State
	}
	for _, d := range descendants(p, ordinal) {
		if state[d] == StateReverted {
			return d, true
		}
	}
	return 0, false
}

// descendants are the steps that depend on step ordinal, in ascending
// ordinal order.
func descendants(p Plan, ordinal int) []int {
	seen := map[int]bool{}
	queue := []int{ordinal}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range p.Edges {
			if e.From == n && !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	out := make([]int, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// reverseTopo is the reverse of the order the engine runs p's steps in:
// a topological order that takes the lowest ready ordinal first.
func reverseTopo(p Plan) []int {
	indegree := map[int]int{}
	for _, s := range p.Steps {
		indegree[s.Ordinal] += 0
	}
	for _, e := range p.Edges {
		indegree[e.To]++
	}
	var order []int
	for len(order) < len(indegree) {
		next := -1
		for n, d := range indegree {
			if d == 0 && !slices.Contains(order, n) && (next < 0 || n < next) {
				next = n
			}
		}
		if next < 0 {
			break // a cycle: validation refuses one
		}
		order = append(order, next)
		for _, e := range p.Edges {
			if e.From == next {
				indegree[e.To]--
			}
		}
	}
	slices.Reverse(order)
	return order
}

// summary is the halt reason once the walk ended: where it went back to,
// what it reverted and what it kept, and why.
func (w walk) summary() string {
	to := "the plan's start"
	if w.checkpoint > 0 {
		to = fmt.Sprintf("checkpoint step %d", w.checkpoint)
	}
	var b strings.Builder
	verdict := w.origin.Runtime.Verdict
	fmt.Fprintf(&b, "step %d %s (on_%s revert_to_checkpoint): ", w.origin.Ordinal, verdict,
		verdict)
	if w.stopped != "" {
		fmt.Fprintf(&b, "the walk back to %s stopped at %s", to, w.stopped)
	} else {
		fmt.Fprintf(&b, "reverted back to %s", to)
	}
	fmt.Fprintf(&b, "; reverted steps %v", w.reverted)
	if len(w.kept) > 0 {
		parts := make([]string, 0, len(w.kept))
		for _, k := range w.kept {
			parts = append(parts, fmt.Sprintf("step %d (%s)", k.Ordinal, k.Why))
		}
		fmt.Fprintf(&b, "; kept %s", strings.Join(parts, ", "))
	}
	return b.String()
}

// detail is the walk's halt event detail.
func (w walk) detail() map[string]any {
	return map[string]any{"walk_from": w.origin.Ordinal, "checkpoint": w.checkpoint,
		"reverted": w.reverted, "kept": w.kept, "stopped": w.stopped}
}
