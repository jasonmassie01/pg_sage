package schema

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config-31: PersistTrustRampStart, split out of bootstrap.go into a read
// and an initialise step with the same SQL.

// trustRampStartTimeout bounds each sage.config statement.
const trustRampStartTimeout = 5 * time.Second

const (
	selectTrustRampStartSQL = "SELECT value FROM sage.config WHERE key = 'trust_ramp_start'"
	insertTrustRampStartSQL = "INSERT INTO sage.config (key, value, updated_by) " +
		"VALUES ('trust_ramp_start', $1, 'bootstrap') " +
		"RETURNING value::timestamptz"
	insertTrustRampStartNowSQL = "INSERT INTO sage.config (key, value, updated_by) " +
		"VALUES ('trust_ramp_start', " +
		"to_char(now(), 'YYYY-MM-DD\"T\"HH24:MI:SS.USOF'), 'bootstrap') " +
		"RETURNING value::timestamptz"
	rereadTrustRampStartSQL = "SELECT value::timestamptz FROM sage.config " +
		"WHERE key = 'trust_ramp_start'"
)

// trustRampStartLayouts are the timestamp formats PG may produce.
var trustRampStartLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999-07",
	"2006-01-02T15:04:05.999999-07:00",
	"2006-01-02 15:04:05.999999-07",
	"2006-01-02 15:04:05.999999-07:00",
}

// PersistTrustRampStart reads or initialises the trust_ramp_start
// timestamp in sage.config, returning the effective start time.
// If the key does not yet exist and configRampStart is non-zero,
// that value is used instead of now().
func PersistTrustRampStart(
	ctx context.Context, pool *pgxpool.Pool, configRampStart time.Time,
) (time.Time, error) {
	t, found, err := readTrustRampStart(ctx, pool)
	if found {
		return t, err
	}
	// Key does not exist — insert configRampStart (if set) or now().
	return initTrustRampStart(ctx, pool, configRampStart)
}

// readTrustRampStart parses the stored trust_ramp_start. found is false
// when the read returns no row or fails: initTrustRampStart then inserts,
// and its re-read reports any failure that persists.
func readTrustRampStart(
	ctx context.Context, pool *pgxpool.Pool,
) (t time.Time, found bool, err error) {
	qctx, cancel := context.WithTimeout(ctx, trustRampStartTimeout)
	defer cancel()

	var raw string
	if pool.QueryRow(qctx, selectTrustRampStartSQL).Scan(&raw) != nil {
		return time.Time{}, false, nil
	}
	for _, layout := range trustRampStartLayouts {
		if parsed, parseErr := time.Parse(layout, raw); parseErr == nil {
			return parsed, true, nil
		}
	}
	return time.Time{}, true, fmt.Errorf(
		"parsing trust_ramp_start %q: no matching format", raw,
	)
}

// initTrustRampStart inserts configRampStart, or the database's now() when
// it is zero. If another instance inserted first, it reads that row.
func initTrustRampStart(
	ctx context.Context, pool *pgxpool.Pool, configRampStart time.Time,
) (time.Time, error) {
	qctx, cancel := context.WithTimeout(ctx, trustRampStartTimeout)
	defer cancel()

	var t time.Time
	var err error
	if !configRampStart.IsZero() {
		err = pool.QueryRow(
			qctx, insertTrustRampStartSQL, configRampStart.Format(time.RFC3339Nano),
		).Scan(&t)
	} else {
		err = pool.QueryRow(qctx, insertTrustRampStartNowSQL).Scan(&t)
	}
	if err == nil {
		return t, nil
	}
	// Race: another instance inserted between our SELECT and INSERT.
	rctx, rcancel := context.WithTimeout(ctx, trustRampStartTimeout)
	defer rcancel()
	if err := pool.QueryRow(rctx, rereadTrustRampStartSQL).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf(
			"reading trust_ramp_start after insert: %w", err,
		)
	}
	return t, nil
}
