package specialist

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// codedError is a contract error: a published code and its HTTP status.
// Callers wrap it with detail (fmt.Errorf("%w: ...")); errors.Is matches
// the sentinel and the MCP layer reads the code (SpecialistCode).
type codedError struct {
	code   string
	status int
	msg    string
}

func (e *codedError) Error() string { return e.msg }

// SpecialistCode is the published error code.
func (e *codedError) SpecialistCode() string { return e.code }

// Contract errors, distinguishable by callers.
var (
	ErrUnauthenticated = &codedError{"unauthenticated", http.StatusUnauthorized,
		"a pg_sage MCP token is required (Authorization: Bearer pgs_mcp_...)"}
	ErrScope = &codedError{"scope_required", http.StatusForbidden,
		"the token lacks the scope this call needs"}
	ErrDatabaseNotPermitted = &codedError{"database_not_permitted",
		http.StatusForbidden, "the token may not name this database"}
	ErrNotFound = &codedError{"not_found", http.StatusNotFound, "not found"}
	ErrInvalid  = &codedError{"invalid_request", http.StatusBadRequest, "invalid request"}
	ErrTooLarge = &codedError{"payload_too_large", http.StatusRequestEntityTooLarge,
		"request body too large"}
	ErrRateLimited = &codedError{"rate_limited", http.StatusTooManyRequests,
		"rate limit exceeded for this token"}
	ErrTooManyInvestigations = &codedError{"too_many_investigations",
		http.StatusTooManyRequests, "too many live investigations opened"}
	ErrNotRequestable = &codedError{"not_requestable", http.StatusConflict,
		"the remediation cannot be requested now"}
	ErrSignature = &codedError{"signature_invalid", http.StatusUnauthorized,
		"webhook signature invalid"}
	ErrUnavailable = &codedError{"unavailable", http.StatusServiceUnavailable,
		"temporarily unavailable"}
	ErrDisabled = &codedError{"disabled", http.StatusNotFound,
		"this adapter is not configured"}
	errInternal = &codedError{"internal", http.StatusInternalServerError, "internal error"}
)

// ErrNoActions is a backend's answer when a database has no action
// service: no cancel proposals exist (not an outage).
var ErrNoActions = errors.New("specialist: no action service for this database")

// RateLimitError is a rate-limit refusal with the wait before a retry.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: retry after %s", ErrRateLimited.msg, e.RetryAfter.Round(
		time.Second))
}

// Unwrap makes errors.Is(err, ErrRateLimited) hold.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// codeOf is the published code and HTTP status of err; anything uncoded is
// internal.
func codeOf(err error) *codedError {
	var c *codedError
	if errors.As(err, &c) {
		return c
	}
	return errInternal
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
