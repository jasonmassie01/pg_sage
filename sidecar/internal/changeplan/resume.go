package changeplan

import (
	"fmt"
	"time"
)

// Resume after a restart or a leader change (plan-02e). A step left in
// flight is read back the index-replace way: what may have happened is
// read from the queue item and the action ledger, never guessed. Starting
// with no queue item starts again; starting with its own pending item
// adopts it; queued runs through RunApprovedAction only while the ledger
// holds no run of its SQL since the step started; running with its one
// ledger row, and executed, go on to judging (a failed run fails);
// reverting reads its revert back and finishes or fails. Anything else is
// halted with interrupted_unknown, and the plan halts with one
// notification: a person reads it. Another engine's live claim on a step
// is never taken: a new leader waits until the claim expires.

// ReasonInterruptedUnknown is why a step resume could not decide halted.
const ReasonInterruptedUnknown = "interrupted_unknown"

// ledgerFloorKey is the starting event's record of the ledger's highest
// action id when the step started: a run of the step is above it.
const ledgerFloorKey = "action_log_floor"

// resumedNote marks the events of a resumed step.
const resumedNote = "after a restart or a leader change"

// resumeKind is what resume does with one step.
type resumeKind int

const (
	resumeNone       resumeKind = iota // nothing: the tick handles it
	resumeRetryStart                   // starting, no queue item: start it again
	resumeAdoptQueue                   // starting, its pending item: record and run it
	resumeRunQueued                    // queued, no run in the ledger: run it
	resumeToJudging                    // its run is in the ledger: judge it
	resumeFail                         // its run failed: the step failed
	resumeRevert                       // reverting: read the revert back, finish it
	resumeUnknown                      // cannot decide: halted interrupted_unknown
)

// resumeEvidence is what resume read about one step.
type resumeEvidence struct {
	queueID     int64  // the step's queue item (0: none)
	queueStatus string // its status
	queueItems  int    // queue items carrying the step's identity
	// floorKnown reports that the step's start recorded the ledger floor;
	// the ledger fields are the runs of its SQL above it, not yet any
	// other step's.
	floorKnown    bool
	ledgerID      int64
	ledgerOutcome string
	ledgerAt      *time.Time
	ledgerRows    int
}

type resumeDecision struct {
	kind   resumeKind
	reason string // why, for an unknown decision
}

// restingStates are the states resume leaves to the tick: not started,
// judged or waiting on a verdict, a person or an operator.
var restingStates = map[StepState]bool{
	StatePending: true, StateKept: true, StateReverted: true, StateSkipped: true,
	StateFailed: true, StateHalted: true, StateAwaitingOperator: true,
	StateAwaitingApproval: true, StateJudging: true,
}

// knownState reports whether st is in the step vocabulary.
func knownState(st StepState) bool {
	return restingStates[st] || inFlightStates[st]
}

func unknownMove(format string, args ...any) resumeDecision {
	return resumeDecision{kind: resumeUnknown, reason: fmt.Sprintf(format, args...)}
}

// resumeMove is the resume table: what step s does next given ev.
func resumeMove(s Step, ev resumeEvidence) resumeDecision {
	switch s.Runtime.State {
	case StateStarting:
		return resumeStarting(ev)
	case StateQueued:
		return resumeQueued(ev)
	case StateRunning:
		return resumeRunning(ev)
	case StateExecuted:
		if s.Runtime.ActionLogID == nil {
			return unknownMove("executed with no action recorded")
		}
		return resumeDecision{kind: resumeToJudging}
	case StateReverting:
		return resumeDecision{kind: resumeRevert}
	}
	if restingStates[s.Runtime.State] {
		return resumeDecision{kind: resumeNone}
	}
	return unknownMove("state %q is not one the resume table knows",
		truncate(string(s.Runtime.State), 60))
}

func resumeStarting(ev resumeEvidence) resumeDecision {
	switch {
	case ev.queueItems == 0:
		return resumeDecision{kind: resumeRetryStart}
	case ev.queueItems == 1 && ev.queueStatus == "pending":
		return resumeDecision{kind: resumeAdoptQueue}
	}
	return unknownMove("starting with %d queue item(s) for it (queue item %d is %q)",
		ev.queueItems, ev.queueID, ev.queueStatus)
}

func resumeQueued(ev resumeEvidence) resumeDecision {
	switch {
	case !ev.floorKnown:
		return unknownMove("queued, and its start recorded no ledger mark to read " +
			"its runs from")
	case ev.ledgerRows > 0:
		return unknownMove("queued, but the ledger holds %d run(s) of its SQL since it "+
			"started (action %d)", ev.ledgerRows, ev.ledgerID)
	case ev.queueID == 0:
		return unknownMove("queued without its queue item")
	case ev.queueStatus == "pending" || ev.queueStatus == "approved":
		return resumeDecision{kind: resumeRunQueued}
	}
	return unknownMove("queued, but queue item %d is %q", ev.queueID, ev.queueStatus)
}

func resumeRunning(ev resumeEvidence) resumeDecision {
	switch {
	case !ev.floorKnown:
		return unknownMove("running, and its start recorded no ledger mark to read " +
			"its runs from")
	case ev.ledgerRows == 0:
		return unknownMove("running, and the ledger holds no run of its SQL: the " +
			"change may or may not have happened")
	case ev.ledgerRows > 1:
		return unknownMove("running, and the ledger holds %d runs of its SQL since it "+
			"started", ev.ledgerRows)
	case ev.ledgerOutcome == "failed":
		return resumeDecision{kind: resumeFail}
	}
	return resumeDecision{kind: resumeToJudging}
}

// claimHeldByOther reports another engine's live claim on s at now: resume
// waits for it to expire.
func claimHeldByOther(s Step, now time.Time, owner string) bool {
	rt := s.Runtime
	return rt.ClaimedBy != "" && rt.ClaimedBy != owner && rt.ClaimedUntil != nil &&
		rt.ClaimedUntil.After(now)
}

func (e *Engine) needsResume() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.needResume
}

func (e *Engine) setNeedResume(v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.needResume = v
}
