package probes

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ReasonMissingRole is a probe the session lacks a required predefined
// role for (MissingRoleError).
const ReasonMissingRole = "missing_role"

// pgReasons maps SQLSTATE codes to a typed status and a stable reason.
var pgReasons = map[string]struct {
	status Status
	reason string
}{
	"42501": {StatusNoPrivilege, "insufficient_privilege"},
	"42P01": {StatusUnsupported, "undefined_table"},
	"42883": {StatusUnsupported, "undefined_function"},
	"42703": {StatusUnsupported, "undefined_column"},
	"0A000": {StatusUnsupported, "feature_not_supported"},
	"55000": {StatusUnsupported, "prerequisite_not_met"},
	"57014": {StatusError, "statement_timeout"},
	"55P03": {StatusError, "lock_timeout"},
	"25006": {StatusError, "read_only_violation"},
}

// classify maps a probe error to its typed status and reason code. A
// missing privilege, extension or relation is never read as an empty
// (healthy) answer.
func classify(err error) (Status, string) {
	if err == nil {
		return StatusOK, ""
	}
	var role *MissingRoleError
	if errors.As(err, &role) {
		return StatusNoPrivilege, ReasonMissingRole
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if r, ok := pgReasons[pgErr.Code]; ok {
			return r.status, r.reason
		}
		return StatusError, "query_failed"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return StatusError, "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return StatusError, "canceled"
	default:
		return StatusError, "query_failed"
	}
}
