package analyzer

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// connectionLost reports whether err means the database could not be
// reached rather than that one row was rejected. A batch keeps going past
// a rejected row, but stops on a lost connection: every later row would
// fail the same way, each after its own connect or network timeout, and
// stall the cycle (rules-06).
func connectionLost(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08 connection exception, 53300 too_many_connections,
		// 57P01-57P03 shutdown or cannot connect now. Any other server
		// error (a statement timeout included) concerns this row only.
		return strings.HasPrefix(pgErr.Code, "08") || pgErr.Code == "53300" ||
			pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// advisable is findings minus those excluded from advice, which an upsert
// never persists.
func advisable(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if !excludedFromAdvice(f) {
			out = append(out, f)
		}
	}
	return out
}
