package candidate

import (
	"errors"
	"fmt"
)

// Status is where a candidate stands. Every candidate starts proposed;
// review-03b plans it, a person rejects it, or a later run supersedes it.
// Planned, rejected and superseded are final: a superseded candidate never
// becomes planned.
type Status string

// The candidate statuses (sage.review_candidate.status).
const (
	StatusProposed   Status = "proposed"
	StatusPlanned    Status = "planned"
	StatusRejected   Status = "rejected"
	StatusSuperseded Status = "superseded"
)

// ErrTransition is a status move the contract does not allow.
var ErrTransition = errors.New("review candidate status move not allowed")

// Transition checks a move from one status to another.
func Transition(from, to Status) error {
	if from != StatusProposed {
		return fmt.Errorf("%w: %q -> %q: only a proposed candidate moves", ErrTransition,
			from, to)
	}
	switch to {
	case StatusPlanned, StatusRejected, StatusSuperseded:
		return nil
	}
	return fmt.Errorf("%w: %q -> %q: unknown status", ErrTransition, from, to)
}

// knownStatus reports a status of the contract.
func knownStatus(s Status) bool {
	switch s {
	case StatusProposed, StatusPlanned, StatusRejected, StatusSuperseded:
		return true
	}
	return false
}
