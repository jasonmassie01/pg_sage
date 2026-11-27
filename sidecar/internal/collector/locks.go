package collector

import (
	"context"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Lock wait graph bounds (collection-01). The locks category is stored in
// full every cycle and kept for the snapshot retention, so its size and
// its text are bounded before it is stored.
const (
	// lockRowCap is the most wait-graph rows a snapshot keeps; locksSQL
	// reads one more to detect truncation.
	lockRowCap = 200
	// lockQueryMaxBytes is the most statement text kept per row, after
	// its literals are redacted.
	lockQueryMaxBytes = 256
)

// collectLocks reads the lock wait graph (locksSQL): at most lockRowCap
// rows with redacted statement text, and how many rows past the cap the
// read saw (0 or 1). An empty graph is an empty, non-nil list.
func (c *Collector) collectLocks(ctx context.Context) ([]LockInfo, int, error) {
	rows, err := c.catalogQuery(ctx, locksSQL)
	if err != nil {
		return nil, 0, err
	}
	locks, err := scanLocks(rows)
	if err != nil {
		return nil, 0, err
	}
	kept, dropped := capLocks(locks)
	if dropped > 0 {
		c.logFn("INFO", "collector: lock wait graph truncated at %d rows "+
			"(more waiters or sessions than the snapshot keeps)", lockRowCap)
	}
	return kept, dropped, nil
}

// scanLocks scans locksSQL's rows, redacting each statement's text. It
// closes rows.
func scanLocks(rows pgx.Rows) ([]LockInfo, error) {
	defer rows.Close()
	result := []LockInfo{}
	for rows.Next() {
		var lk LockInfo
		if err := rows.Scan(
			&lk.LockType, &lk.Mode, &lk.Granted, &lk.RelName,
			&lk.Query, &lk.State, &lk.WaitEventType, &lk.WaitEvent, &lk.PID,
			&lk.BackendStart, &lk.QueryStart, &lk.XactStart, &lk.BackendXminAge,
			&lk.BlockedBy, &lk.QueryID, &lk.StateChange,
		); err != nil {
			return nil, err
		}
		lk.Query = redactLockQuery(lk.Query)
		result = append(result, lk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// capLocks keeps the first lockRowCap rows (locksSQL orders the graph's
// roots first) and reports how many it dropped. The result is never nil.
func capLocks(locks []LockInfo) ([]LockInfo, int) {
	if locks == nil {
		return []LockInfo{}, 0
	}
	if len(locks) <= lockRowCap {
		return locks, 0
	}
	return locks[:lockRowCap], len(locks) - lockRowCap
}

// redactLockQuery is q with its string and dollar-quoted literals
// redacted (the same redaction as text sent to an LLM), then cut to
// lockQueryMaxBytes on a UTF-8 boundary. Redacting first means a cut never
// leaves the head of a literal behind. Nil stays nil.
func redactLockQuery(q *string) *string {
	if q == nil {
		return nil
	}
	text := llm.RedactSQLLiterals(*q)
	if len(text) > lockQueryMaxBytes {
		cut := lockQueryMaxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return &text
}
