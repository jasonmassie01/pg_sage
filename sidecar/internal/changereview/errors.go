package changereview

import "errors"

// Errors, distinguishable with errors.Is; details are wrapped.
var (
	// ErrInvalid: the request or verdict is malformed; nothing was stored.
	ErrInvalid = errors.New("changereview: invalid request")
	// ErrNotFound: no such review on this database.
	ErrNotFound = errors.New("changereview: not found")
	// ErrDisabled: change_review.enabled is false.
	ErrDisabled = errors.New("changereview: Change Review is disabled (change_review.enabled)")
	// ErrBusy: the database's runner and its queue are full; retry later.
	ErrBusy = errors.New("changereview: busy")
	// ErrAlreadyDecided: the transition already happened (a DBA verdict,
	// a landing, a receipt or a shadow score is recorded once).
	ErrAlreadyDecided = errors.New("changereview: already decided")
	// ErrUnavailable: the store or a stage could not be reached.
	ErrUnavailable = errors.New("changereview: unavailable")
	// ErrBudgetExhausted: a daily token allocation cannot pay for a call.
	ErrBudgetExhausted = errors.New("changereview: daily budget exhausted")
)
