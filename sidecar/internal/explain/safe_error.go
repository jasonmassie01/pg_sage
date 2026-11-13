package explain

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Server text that can quote column values never reaches an agent or a
// model (security-10). A data exception (SQLSTATE class 22) quotes the
// value it could not convert; an integrity constraint violation (class
// 23) quotes the key in its detail. The agent broker withholds the same
// class 22 text in its own path (agentguard/broker dbFailure).

// WithheldDataMessage replaces the message of a class 22 server error.
const WithheldDataMessage = "(withheld: a data exception can quote column values)"

// WithheldConstraintMessage replaces the message of a class 23 server
// error.
const WithheldConstraintMessage = "(withheld: a constraint violation can quote key values)"

// SafeError is err as an agent or a model may see it. When err carries a
// *pgconn.PgError of SQLSTATE class 22 or 23, the result carries a copy
// whose Message is withheld and whose Detail, Hint, Where and
// InternalQuery are cleared; the SQLSTATE and errors.Is on err's other
// wrapped errors are kept. Any other error is returned unchanged. The
// caller logs the original for the operator.
func SafeError(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr == nil {
		return err
	}
	message := withheldMessage(pgErr.Code)
	if message == "" {
		return err
	}
	safe := *pgErr
	safe.Message, safe.Detail, safe.Hint, safe.Where = message, "", "", ""
	safe.InternalQuery, safe.InternalPosition = "", 0
	return &withheldError{safe: &safe, orig: err, text: safeText(err, pgErr, &safe)}
}

// withheldMessage is the replacement message for code, or "" when the
// code's text is passed through.
func withheldMessage(code string) string {
	switch {
	case strings.HasPrefix(code, "22"):
		return WithheldDataMessage
	case strings.HasPrefix(code, "23"):
		return WithheldConstraintMessage
	}
	return ""
}

// safeText is err's text with the server error's text replaced by safe's,
// so a caller's context prefix survives. When any withheld field still
// appears (a wrapper quoted the message itself), only safe's text is kept.
func safeText(err error, pgErr, safe *pgconn.PgError) string {
	text := strings.ReplaceAll(err.Error(), pgErr.Error(), safe.Error())
	for _, field := range []string{pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where,
		pgErr.InternalQuery} {
		if field != "" && strings.Contains(text, field) {
			return safe.Error()
		}
	}
	return text
}

// withheldError hides the original error: errors.As finds only the safe
// copy, while errors.Is still matches the original's sentinels.
type withheldError struct {
	safe *pgconn.PgError
	orig error
	text string
}

func (e *withheldError) Error() string { return e.text }

func (e *withheldError) Unwrap() error { return e.safe }

// Is matches the sentinels the original error wraps.
func (e *withheldError) Is(target error) bool { return errors.Is(e.orig, target) }
