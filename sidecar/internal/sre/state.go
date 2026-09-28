package sre

// State is an investigation lifecycle state (Codex §7). Investigation
// state is separate from incident state.
type State string

// Investigation states.
const (
	StateQueued        State = "queued"
	StateCollecting    State = "collecting"
	StateEvaluating    State = "evaluating"
	StateNeedsEvidence State = "needs_evidence"
	StateConcluded     State = "concluded"
	StateInconclusive  State = "inconclusive"
	StatePaused        State = "paused"
	StateCancelled     State = "cancelled"
	StateExpired       State = "expired"
	StateFailed        State = "failed"
)

// interrupted are the states any active state may move to.
var interrupted = []State{StatePaused, StateCancelled, StateExpired, StateFailed}

var transitions = map[State][]State{
	StateQueued:     append([]State{StateCollecting}, interrupted...),
	StateCollecting: append([]State{StateCollecting, StateEvaluating}, interrupted...),
	StateEvaluating: append([]State{StateNeedsEvidence, StateConcluded,
		StateInconclusive}, interrupted...),
	StateNeedsEvidence: append([]State{StateCollecting}, interrupted...),
	StatePaused:        {StateQueued, StateCancelled, StateExpired},
}

// AllStates lists every state.
func AllStates() []State {
	return []State{StateQueued, StateCollecting, StateEvaluating, StateNeedsEvidence,
		StateConcluded, StateInconclusive, StatePaused, StateCancelled, StateExpired,
		StateFailed}
}

// CanTransition reports whether from may move to to.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Terminal reports a final state.
func (s State) Terminal() bool {
	switch s {
	case StateConcluded, StateInconclusive, StateCancelled, StateExpired, StateFailed:
		return true
	}
	return false
}

// Live reports a state that holds its trigger (one live investigation
// per scoped trigger fingerprint).
func (s State) Live() bool {
	switch s {
	case StateQueued, StateCollecting, StateEvaluating, StateNeedsEvidence, StatePaused:
		return true
	}
	return false
}
