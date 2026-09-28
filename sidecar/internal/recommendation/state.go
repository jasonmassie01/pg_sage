package recommendation

// State is a recommendation's lifecycle state.
type State string

// The lifecycle: proposed → approved → applying → applied → verifying →
// verified | reverted | inconclusive, plus superseded, failed, abandoned.
const (
	StateProposed     State = "proposed"
	StateApproved     State = "approved"
	StateApplying     State = "applying"
	StateApplied      State = "applied"
	StateVerifying    State = "verifying"
	StateVerified     State = "verified"
	StateReverted     State = "reverted"
	StateInconclusive State = "inconclusive"
	StateSuperseded   State = "superseded"
	StateFailed       State = "failed"
	StateAbandoned    State = "abandoned"
)

// transitions lists every legal edge. Nothing transitions into proposed:
// only a new revision re-proposes (Propose), so a failure can never
// silently return to proposed (decision a).
var transitions = map[State][]State{
	StateProposed:  {StateApproved, StateSuperseded},
	StateApproved:  {StateApplying, StateSuperseded},
	StateApplying:  {StateApplied, StateFailed},
	StateApplied:   {StateVerifying},
	StateVerifying: {StateVerified, StateReverted, StateInconclusive},
	StateFailed:    {StateApplying, StateAbandoned, StateSuperseded},
}

// AllStates lists every state.
func AllStates() []State {
	return []State{StateProposed, StateApproved, StateApplying, StateApplied,
		StateVerifying, StateVerified, StateReverted, StateInconclusive,
		StateSuperseded, StateFailed, StateAbandoned}
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	for _, known := range AllStates() {
		if s == known {
			return true
		}
	}
	return false
}

// Terminal reports whether s ends the recommendation's life.
func (s State) Terminal() bool {
	switch s {
	case StateVerified, StateReverted, StateInconclusive, StateSuperseded, StateAbandoned:
		return true
	default:
		return false
	}
}

// CanTransition reports whether from → to is a legal edge.
func CanTransition(from, to State) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// CanRevise reports whether a new revision may replace the content of a
// recommendation in state s. In-flight and terminal rows keep theirs.
func CanRevise(s State) bool {
	return s == StateProposed || s == StateApproved || s == StateFailed
}

// liveStates are the non-terminal states, as SQL literals for predicates.
const liveStatesSQL = `('proposed', 'approved', 'applying', 'applied', 'verifying', 'failed')`

// revisableStatesSQL are the states CanRevise accepts, for SQL predicates.
const revisableStatesSQL = `('proposed', 'approved', 'failed')`
