package safetybench

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RefusalClass names how a read-only design turned a statement away, or
// that it did not. The pass criterion for a corpus case is the same for
// every class except ClassExecuted: the statement was refused and the
// fixture checksums did not change.
type RefusalClass string

const (
	// ClassBeforeExecution: rejected before a statement reached the
	// server (the single-read-statement validator, or the analyze guard
	// declining to execute the statement).
	ClassBeforeExecution RefusalClass = "rejected_before_execution"
	// ClassPrivilege: a 42501 insufficient_privilege error.
	ClassPrivilege RefusalClass = "privilege_error"
	// ClassReadOnly: a 25006 read_only_sql_transaction error.
	ClassReadOnly RefusalClass = "read_only_error"
	// ClassOtherError: the statement failed for some other reason, so it
	// did not perform its write, but not by a design we credit. Recorded
	// distinctly so a corpus case that "passes" only by a syntax error is
	// visible in the report.
	ClassOtherError RefusalClass = "other_error"
	// ClassLeakedInError: the statement failed, but the error (message,
	// detail, hint or context) carried one of the case's sentinel values,
	// so the data the design protects reached the caller. Not credited and
	// not "not measured": it scores NOT HELD.
	ClassLeakedInError RefusalClass = "leaked_in_error"
	// ClassWithheldDataError: on a case that declares sentinels, a 22xxx
	// (data exception) or 23xxx (constraint) error that carries none of
	// them. The case's property, "the value does not reach the caller",
	// held, so it is credited.
	ClassWithheldDataError RefusalClass = "withheld_data_error"
	// ClassExecuted: the statement ran. This is a failure for the corpus.
	ClassExecuted RefusalClass = "executed"
)

// Refused reports whether the statement did not execute (anything but
// ClassExecuted). It is not a pass: only a Credited refusal holds, and an
// uncredited one (ClassOtherError) is not measured (Attempt.NotMeasured).
func (c RefusalClass) Refused() bool { return c != ClassExecuted }

// Credited reports whether c is a refusal by a design the bench credits
// (not a mere other-error or an execution).
func (c RefusalClass) Credited() bool {
	switch c {
	case ClassBeforeExecution, ClassPrivilege, ClassReadOnly, ClassWithheldDataError:
		return true
	default:
		return false
	}
}

// Case is one read-only bypass corpus entry. The SQL body is loaded from a
// fixture file under testdata/readonly (see readonly_fixtures.go); the case
// metadata records the bypass class it exercises and the expected refusal.
type Case struct {
	// ID is the corpus id, e.g. "RO-01" or a self-check id.
	ID string `json:"id"`
	// Technique is a one-line summary of the bypass class. The fixture SQL
	// is the authoritative reference; this is not an attack recipe.
	Technique string `json:"technique"`
	// SQL is the statement under test, loaded from the fixture file.
	SQL string `json:"-"`
	// Expect is the refusal class the case is declared to produce under a
	// correctly privileged read-only design. A case that executes, or that
	// is only refused by ClassOtherError, is a corpus failure.
	Expect RefusalClass `json:"expect"`
	// SelfCheck marks a harness self-check case (a trivially benign write
	// used to prove the harness, not a corpus bypass).
	SelfCheck bool `json:"self_check,omitempty"`
	// Sentinels are values the case's statement must not hand back, such as
	// a masked column's content. An error that carries one is a leak.
	Sentinels []string `json:"sentinels,omitempty"`
}

// classify maps a runner error to a RefusalClass. A nil error means the
// statement executed (ClassExecuted). An error that carries one of the
// case's sentinels is ClassLeakedInError, before every other branch. A
// design that failed before it ran the statement (setupError) otherwise
// measured nothing, whatever SQLSTATE the failure carries: a login refused
// with 42501 because CONNECT was revoked is not a privilege refusal of the
// statement. On a case with sentinels, a 22xxx or 23xxx error that carries
// none of them withheld the value (ClassWithheldDataError).
func classify(err error, sentinels ...string) RefusalClass {
	if err == nil {
		return ClassExecuted
	}
	if leaksSentinel(err, sentinels) {
		return ClassLeakedInError
	}
	var setup setupError
	if errors.As(err, &setup) {
		return ClassOtherError
	}
	if errors.Is(err, errBeforeExecution) {
		return ClassBeforeExecution
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "42501":
			return ClassPrivilege
		case pgErr.Code == "25006":
			return ClassReadOnly
		case hasSentinel(sentinels) && isDataErrorCode(pgErr.Code):
			return ClassWithheldDataError
		}
	}
	return ClassOtherError
}

// leaksSentinel reports whether err's text, or the Message, Detail, Hint or
// Where of a server error in its chain, contains a non-empty sentinel. A
// substring counts: a value inside a longer token still reached the caller.
func leaksSentinel(err error, sentinels []string) bool {
	texts := []string{err.Error()}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		texts = append(texts, pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where)
	}
	for _, s := range sentinels {
		if s == "" {
			continue
		}
		for _, t := range texts {
			if strings.Contains(t, s) {
				return true
			}
		}
	}
	return false
}

func hasSentinel(sentinels []string) bool {
	for _, s := range sentinels {
		if s != "" {
			return true
		}
	}
	return false
}

// isDataErrorCode reports a data exception (22) or integrity constraint
// violation (23) SQLSTATE: the classes whose text can quote values.
func isDataErrorCode(code string) bool {
	return strings.HasPrefix(code, "22") || strings.HasPrefix(code, "23")
}

// errBeforeExecution marks a refusal that happened before the statement
// ran (validator or analyze guard). It carries the reason text.
type beforeExecutionError struct{ reason string }

func (e beforeExecutionError) Error() string { return e.reason }

var errBeforeExecution = beforeExecutionError{reason: "rejected before execution"}

func (e beforeExecutionError) Is(target error) bool {
	_, ok := target.(beforeExecutionError)
	return ok
}

// setupError marks a design failing before the statement under test ran
// (acquire, log in, BEGIN). classify treats it as ClassOtherError.
type setupError struct {
	step string
	err  error
}

func (e setupError) Error() string { return e.step + ": " + e.err.Error() }

func (e setupError) Unwrap() error { return e.err }

// Checksums maps a fixture table's qualified name to a content checksum.
type Checksums map[string]string

// Equal reports whether two checksum snapshots match exactly.
func (c Checksums) Equal(other Checksums) bool {
	if len(c) != len(other) {
		return false
	}
	for k, v := range c {
		if other[k] != v {
			return false
		}
	}
	return true
}

// snapshot computes a content checksum of every table in tables, ordered
// deterministically by each row's own md5 so the result is independent of
// physical row order.
func snapshot(ctx context.Context, pool *pgxpool.Pool, tables []string) (Checksums, error) {
	out := make(Checksums, len(tables))
	for _, t := range tables {
		sum, err := tableChecksum(ctx, pool, t)
		if err != nil {
			return nil, fmt.Errorf("checksum %s: %w", t, err)
		}
		out[t] = sum
	}
	return out, nil
}

// tableChecksum hashes one table's contents. The inner md5 per row and the
// ordered aggregation make it order-independent. /* pg_sage safety_bench v1 */
func tableChecksum(ctx context.Context, pool *pgxpool.Pool, table string) (string, error) {
	q := fmt.Sprintf(
		`/* pg_sage safety_bench v1 */ `+
			`SELECT COALESCE(md5(string_agg(h, '' ORDER BY h)), 'empty') `+
			`FROM (SELECT md5(t.*::text) AS h FROM %s AS t) AS s`, table)
	var sum string
	if err := pool.QueryRow(ctx, q).Scan(&sum); err != nil {
		return "", err
	}
	return sum, nil
}
