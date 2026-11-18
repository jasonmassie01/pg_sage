package modelgate

import (
	"errors"
	"fmt"
)

// State is a rule's state for one model, as its latest sage.review_gate
// row records it ("" is no row yet: a new model starts fresh).
type State string

// G-M1 moves not_eligible -> eligible -> pending_approval -> granted ->
// revoked; G-M2 moves between model_on and model_off.
const (
	StateNotEligible State = "not_eligible"
	StateEligible    State = "eligible"
	StatePending     State = "pending_approval"
	StateGranted     State = "granted"
	StateRevoked     State = "revoked"
	StateModelOn     State = "model_on"
	StateModelOff    State = "model_off"
)

// Event is what moves G-M1.
type Event string

// The G-M1 events: an evaluation that passes or fails, the promotion filed
// in the inbox, a person's approval, and a change of the running model.
const (
	EventPass         Event = "pass"
	EventFail         Event = "fail"
	EventFiled        Event = "filed"
	EventApproved     Event = "approved"
	EventModelChanged Event = "model_changed"
)

// ErrNotPending is an approval of anything but a pending promotion.
var ErrNotPending = errors.New("model gate: only a pending G-M1 promotion can be approved")

// NextM1 is G-M1's transition. Only an approval of a pending promotion
// reaches granted; a failed window or a model change takes a grant to
// revoked, and anything not granted back to not_eligible.
func NextM1(s State, e Event) (State, error) {
	switch s {
	case "", StateNotEligible, StateEligible, StatePending, StateGranted, StateRevoked:
	default:
		return s, fmt.Errorf("model gate: %q is not a G-M1 state", s)
	}
	switch e {
	case EventApproved:
		if s != StatePending {
			return s, fmt.Errorf("%w (state %q)", ErrNotPending, s)
		}
		return StateGranted, nil
	case EventFail, EventModelChanged:
		if s == StateGranted || s == StateRevoked {
			return StateRevoked, nil
		}
		return StateNotEligible, nil
	case EventPass:
		if s == StatePending || s == StateGranted {
			return s, nil
		}
		return StateEligible, nil
	case EventFiled:
		if s == StateEligible {
			return StatePending, nil
		}
		return s, fmt.Errorf("model gate: nothing eligible to file in state %q", s)
	}
	return s, fmt.Errorf("model gate: unknown G-M1 event %q", e)
}

// NextM2 is G-M2's state: model_off when the rule drops the model and no
// operator set review.model_pass on, else model_on. The rule is evaluated
// anew each tick, so model_off lasts only while its evidence holds.
func NextM2(_ State, drop, operatorOn bool) State {
	if drop && !operatorOn {
		return StateModelOff
	}
	return StateModelOn
}
