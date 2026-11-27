package modelgate

import (
	"errors"
	"testing"
)

// The state machines are pure. No concurrent access tests here: they hold
// no state.

func TestNextM1_FullPathAndBothRevocations(t *testing.T) {
	steps := []struct {
		from  State
		event Event
		to    State
	}{
		{StateNotEligible, EventPass, StateEligible},
		{StateEligible, EventFiled, StatePending},
		{StatePending, EventPass, StatePending},
		{StatePending, EventApproved, StateGranted},
		{StateGranted, EventPass, StateGranted},
		{StateGranted, EventFail, StateRevoked},
		{StateRevoked, EventPass, StateEligible},
		{StateGranted, EventModelChanged, StateRevoked},
		{StatePending, EventFail, StateNotEligible},
		{StatePending, EventModelChanged, StateNotEligible},
		{StateEligible, EventFail, StateNotEligible},
		{StateNotEligible, EventFail, StateNotEligible},
		{StateRevoked, EventFail, StateRevoked},
		{"", EventPass, StateEligible},
		{"", EventFail, StateNotEligible},
	}
	for _, s := range steps {
		got, err := NextM1(s.from, s.event)
		if err != nil || got != s.to {
			t.Errorf("%q --%s--> %q (%v), want %q", s.from, s.event, got, err, s.to)
		}
	}
}

// A grant needs a pending item that a person approved: no other state
// reaches granted.
func TestNextM1_ApprovalOnlyFromPending(t *testing.T) {
	for _, from := range []State{"", StateNotEligible, StateEligible, StateRevoked,
		StateGranted} {
		got, err := NextM1(from, EventApproved)
		// On a refusal the state is unchanged (granted stays granted).
		if !errors.Is(err, ErrNotPending) || got != from {
			t.Errorf("approve from %q: %q %v, want ErrNotPending", from, got, err)
		}
	}
	for _, from := range []State{"", StateNotEligible, StatePending, StateRevoked} {
		if got, _ := NextM1(from, EventPass); got == StateGranted {
			t.Errorf("a pass from %q granted authority by itself", from)
		}
	}
	if _, err := NextM1(StateModelOff, EventPass); err == nil {
		t.Error("a G-M2 state was accepted by G-M1")
	}
	if _, err := NextM1(StatePending, Event("nudge")); err == nil {
		t.Error("an unknown event was accepted")
	}
}

func TestNextM2_OperatorOnRestoresAndNewModelResets(t *testing.T) {
	cases := []struct {
		from       State
		drop, opOn bool
		to         State
	}{
		{"", false, false, StateModelOn},
		{"", true, false, StateModelOff},
		{StateModelOn, true, false, StateModelOff},
		{StateModelOff, true, false, StateModelOff},
		{StateModelOff, false, false, StateModelOn},
		{StateModelOff, true, true, StateModelOn},
		{StateModelOn, true, true, StateModelOn},
	}
	for _, c := range cases {
		if got := NextM2(c.from, c.drop, c.opOn); got != c.to {
			t.Errorf("%q drop=%v on=%v: %q, want %q", c.from, c.drop, c.opOn, got, c.to)
		}
	}
}
