package changeplan

import (
	"regexp"
	"slices"
	"time"
)

// moveKind is one engine decision on a plan.
type moveKind int

const (
	moveWait       moveKind = iota // nothing to do this tick
	moveJudgeStart                 // executed -> judging
	moveJudge                      // read the verdict of a judging step
	moveSkip                       // pending -> skipped (a predecessor was reverted)
	moveCheck                      // pending -> kept (a check step)
	moveOperator                   // pending -> awaiting_operator (plan-02d)
	moveComplete                   // every step ended: the plan is completed
	moveHalt                       // a step failed, halted or regressed
	moveStart                      // start the next runnable action step
	moveRevert                     // carry on a reverting step's revert (plan-02c)
	moveWalkRevert                 // kept -> reverting: the walk's next step
)

// move is decide's answer: what to do, to which step and why.
type move struct {
	kind   moveKind
	step   Step
	reason string
	walk   *walk // the revert_to_checkpoint walk the move belongs to
}

// inFlightStates are the states of a started step without a verdict (or
// being reverted): while one exists no other step of the plan starts.
var inFlightStates = map[StepState]bool{
	StateStarting: true, StateQueued: true, StateRunning: true, StateExecuted: true,
	StateJudging: true, StateReverting: true,
}

// endedStates are the states a step finishes in without halting the plan.
var endedStates = map[StepState]bool{
	StateKept: true, StateReverted: true, StateSkipped: true,
}

// Skip reasons.
const (
	ReasonPredecessorReverted = "predecessor_reverted"
	ReasonPredecessorSkipped  = "predecessor_skipped"
	ReasonRevertedByVerifier  = "reverted_by_verifier"
	ReasonRevertedOutside     = "reverted_outside_plan"
)

// decide is the plan's next move, a pure function of its rows. A step in
// flight is judged, reverted (or waited on) first; then a
// revert_to_checkpoint walk goes on (plan-02c); a failed, halted or
// regressed step halts the plan; then predecessors' reverts are
// propagated; then the plan completes, or its lowest runnable step starts.
// owner's claims and lapsed claims may be judged; another live engine's
// may not.
func decide(p Plan, now time.Time, owner string) move {
	for _, s := range p.Steps {
		if !inFlightStates[s.Runtime.State] {
			continue
		}
		claimable := s.Runtime.ClaimedBy == "" || s.Runtime.ClaimedBy == owner ||
			s.Runtime.ClaimedUntil == nil || !s.Runtime.ClaimedUntil.After(now)
		switch {
		case s.Runtime.State == StateExecuted && claimable:
			return move{kind: moveJudgeStart, step: s}
		case s.Runtime.State == StateJudging && claimable:
			return move{kind: moveJudge, step: s}
		case s.Runtime.State == StateReverting && claimable:
			return move{kind: moveRevert, step: s}
		}
		return move{kind: moveWait, step: s, reason: "step in flight"}
	}
	if m, ok := walkMove(p); ok {
		return m
	}
	for _, s := range p.Steps {
		if why := haltReason(s); why != "" {
			return move{kind: moveHalt, step: s, reason: why}
		}
		switch s.Runtime.State {
		case StateAwaitingOperator:
			return move{kind: moveWait, step: s, reason: "awaiting the operator"}
		case StateAwaitingApproval:
			return move{kind: moveWait, step: s, reason: "awaiting its own approval"}
		}
	}
	return nextPending(p)
}

// nextPending propagates a predecessor's revert, completes an ended plan
// or picks its lowest runnable pending step.
func nextPending(p Plan) move {
	byOrdinal := make(map[int]Step, len(p.Steps))
	for _, s := range p.Steps {
		byOrdinal[s.Ordinal] = s
	}
	pending := 0
	var runnable *Step
	for i, s := range p.Steps {
		if s.Runtime.State != StatePending {
			continue
		}
		pending++
		ready := true
		for _, e := range p.Edges {
			if e.To != s.Ordinal {
				continue
			}
			switch byOrdinal[e.From].Runtime.State {
			case StateReverted:
				return move{kind: moveSkip, step: s, reason: ReasonPredecessorReverted}
			case StateSkipped:
				return move{kind: moveSkip, step: s, reason: ReasonPredecessorSkipped}
			case StateKept:
			default:
				ready = false
			}
		}
		if ready && runnable == nil {
			runnable = &p.Steps[i]
		}
	}
	switch {
	case pending == 0:
		return move{kind: moveComplete}
	case runnable == nil:
		return move{kind: moveHalt, reason: "no pending step can run: its predecessors " +
			"can never be kept"}
	case runnable.Kind == KindCheck:
		return move{kind: moveCheck, step: *runnable}
	case runnable.Kind == KindOperator:
		return move{kind: moveOperator, step: *runnable}
	}
	return move{kind: moveStart, step: *runnable}
}

// haltReason is why step s halts its plan ("" when it does not): it
// failed or halted, regressed (whatever its action did), or was reverted
// on a verdict whose action asks a person.
func haltReason(s Step) string {
	switch {
	case s.Runtime.State == StateFailed:
		return "step failed: " + s.Runtime.StateReason
	case s.Runtime.State == StateHalted:
		return "step halted on verdict " + string(s.Runtime.Verdict) + ": " +
			s.Runtime.StateReason
	case s.Runtime.Verdict == OutcomeRegressed:
		return "step regressed and was " + string(s.Runtime.State)
	case s.Runtime.State == StateReverted && s.ActionFor(s.Runtime.Verdict) == ActionHaltAsk:
		return "step reverted on verdict " + string(s.Runtime.Verdict) +
			", which asks a person"
	case s.Runtime.State == StateSkipped && s.Runtime.Verdict == OutcomeExpired &&
		s.ActionFor(OutcomeExpired) != ActionKeep &&
		s.ActionFor(OutcomeExpired) != ActionRevertStep:
		// halt_ask; revert_to_checkpoint walks first (walkMove).
		return "step expired waiting on a plan cap or a peak window: " +
			s.Runtime.StateReason
	}
	return ""
}

// judgeOutcome reads verdict v for executed step s at now. done is false
// while the verdict is pending and the deadline has not passed. A verdict
// decided after the deadline is not used: the step expired. byVerifier
// reports an action the verifier (or anyone outside the plan) already
// rolled back.
func judgeOutcome(s Step, v ActionVerdict, now time.Time) (o Outcome, byVerifier,
	done bool) {
	deadline := now
	if s.Runtime.ExecutedAt != nil {
		deadline = s.Runtime.ExecutedAt.Add(time.Duration(s.DeadlineMinutes) * time.Minute)
	}
	final := v.Verdict != "" && v.Verdict != "pending"
	switch v.ActionOutcome {
	case "rolled_back", "reverted":
		if !final {
			return OutcomeInconclusive, true, true
		}
		return MapVerifierOutcome(v.Verdict), true, true
	case "verification_expired":
		return OutcomeExpired, false, true
	}
	if final && (v.DecidedAt == nil || !v.DecidedAt.After(deadline)) {
		return MapVerifierOutcome(v.Verdict), false, true
	}
	if now.After(deadline) {
		return OutcomeExpired, false, true
	}
	return "", false, false
}

// settingSQL matches a setting change: ALTER SYSTEM, or ALTER DATABASE,
// ROLE or USER ... SET / RESET.
var settingSQL = regexp.MustCompile(
	`(?is)^\s*ALTER\s+(SYSTEM\s|(DATABASE|ROLE|USER)\s.*\b(SET|RESET)\b)`)

// isSetting reports whether s changes a setting (it moves every query, so
// it runs alone on the database).
func isSetting(s Step) bool { return settingSQL.MatchString(s.ProposedSQL) }

// otherStep is a step of another plan in flight on the database.
type otherStep struct {
	planID  int64
	ordinal int
	targets []int64
	sql     string
}

// conflict is the other plan's step that keeps s from starting, if any:
// one sharing a target query, any step when s is a setting, and a setting
// step whatever s is.
func conflict(s Step, others []otherStep) (otherStep, bool) {
	for _, o := range others {
		if isSetting(s) || settingSQL.MatchString(o.sql) {
			return o, true
		}
		for _, q := range s.TargetQueryIDs {
			if slices.Contains(o.targets, q) {
				return o, true
			}
		}
	}
	return otherStep{}, false
}
