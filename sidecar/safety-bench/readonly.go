package safetybench

import (
	"context"
	"errors"
	"fmt"

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
	// ClassExecuted: the statement ran. This is a failure for the corpus.
	ClassExecuted RefusalClass = "executed"
)

// Refused reports whether c counts as the statement being turned away
// (anything but having executed).
func (c RefusalClass) Refused() bool { return c != ClassExecuted }

// Credited reports whether c is a refusal by a design the bench credits
// (not a mere other-error or an execution).
func (c RefusalClass) Credited() bool {
	switch c {
	case ClassBeforeExecution, ClassPrivilege, ClassReadOnly:
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
}

// classify maps a runner error to a RefusalClass. A nil error means the
// statement executed (ClassExecuted).
func classify(err error) RefusalClass {
	if err == nil {
		return ClassExecuted
	}
	if errors.Is(err, errBeforeExecution) {
		return ClassBeforeExecution
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501":
			return ClassPrivilege
		case "25006":
			return ClassReadOnly
		}
	}
	return ClassOtherError
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
