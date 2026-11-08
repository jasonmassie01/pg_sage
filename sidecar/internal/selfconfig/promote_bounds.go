package selfconfig

import "fmt"

// dropOtherVersion drops what another version of the rule derived (the
// promoted value and any soak): the new version earns its own value. A
// state that records no version (never stored) is kept.
func (s *stepper) dropOtherVersion() {
	was := s.in.Prev.RuleVersion
	if was == 0 || was == s.in.Rule.Version {
		return
	}
	previous := s.next.Active
	if previous == nil {
		previous = s.next.Shadow
	}
	if previous == nil {
		return
	}
	s.emit(EventCleared, ptr(s.in.Default), ptr(*previous), fmt.Sprintf(
		"rule %s changed from version %d to version %d; the new version soaks its "+
			"own value", s.in.Rule.Name, was, s.in.Rule.Version))
	s.next.Active = nil
	s.next.clearShadow()
}

// clampActive keeps the promoted value inside the rule's current bounds
// (another key, a bounds or a rule change can narrow them): it is moved
// inside them, or dropped when they are empty, with a ledger row.
func (s *stepper) clampActive() {
	b := s.next.Bounds
	if s.next.Active == nil || b == (Bounds{}) {
		return
	}
	old := *s.next.Active
	if b.Min > b.Max {
		s.emit(EventCleared, ptr(s.in.Default), ptr(old), fmt.Sprintf("the bounds are "+
			"empty (%g > %g): %s; the derived value %g is dropped", b.Min, b.Max, b.Note, old))
		s.next.Active = nil
		return
	}
	v := Clamp(old, b)
	if equal(v, old) {
		return
	}
	note := ""
	if b.Note != "" {
		note = " (" + b.Note + ")"
	}
	s.emit(EventClamped, ptr(v), ptr(old), fmt.Sprintf("the bounds are now %g-%g%s; the "+
		"derived value %g is clamped to %g", b.Min, b.Max, note, old, v))
	s.next.Active = ptr(v)
}
