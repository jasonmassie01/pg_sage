package earned

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// The SQL of the major cap (earned-07, major_cap.go).

// WithServerMajor gives the ledger the monitored server's major, which
// every level write records (the limiter's Binding.ServerMajor checks
// it). Without one, writes record none and the limiter stamps the rows.
func (s *Service) WithServerMajor(src MajorSource) {
	if src == nil {
		s.store.major.Store(nil)
		return
	}
	s.store.major.Store(&src)
}

// stampMajor sets st's server_major to to if the row is still at st's
// version and its major is want (nil: NULL). The level and version do
// not change; false when the row moved on (retried by the next call).
func (s *PostgresStore) stampMajor(ctx context.Context, q querier, st State, want *int,
	to int) (bool, error) {
	var expect any
	if want != nil {
		expect = int16(*want)
	}
	tag, err := q.Exec(ctx, `UPDATE sage.sre_family_autonomy SET server_major = $5
		WHERE deployment_id = $1 AND database_name = $2 AND family = $3
		  AND action_class = $4 AND version = $6
		  AND server_major IS NOT DISTINCT FROM $7::smallint`,
		s.deployment, s.database, string(st.Family), string(st.Class), int16(to),
		st.Version, expect)
	if err != nil {
		return false, storeErr("stamp the server major", err)
	}
	return tag.RowsAffected() == 1, nil
}

// improvedSinceMajorCapSQL counts a pair's improved outcomes recorded
// after its major cap to $5: the oldest capped event naming that major
// since the last cap entry that names none (the end of an earlier cap).
const improvedSinceMajorCapSQL = `/* pg_sage */ WITH anchor AS (
	SELECT min(e.created_at) AS at FROM sage.sre_autonomy_events e
	 WHERE e.deployment_id = $1 AND e.database_name = $2 AND e.family = $3
	   AND e.action_class = $4 AND e.event_type = 'capped'
	   AND e.evidence -> 'server_major' ->> 'to' = $5
	   AND e.created_at > COALESCE((SELECT max(b.created_at)
	         FROM sage.sre_autonomy_events b
	        WHERE b.deployment_id = $1 AND b.database_name = $2 AND b.family = $3
	          AND b.action_class = $4 AND b.event_type IN ('capped', 'cap_cleared')
	          AND NOT b.evidence ? 'server_major'), '-infinity'))
	SELECT count(*) FROM sage.sre_autonomy_outcomes o, anchor
	 WHERE anchor.at IS NOT NULL AND o.deployment_id = $1 AND o.database_name = $2
	   AND o.family = $3 AND o.action_class = $4 AND o.verdict = 'improved'
	   AND o.recorded_at > anchor.at`

// improvedSinceMajorCap counts the pair's improved outcomes since its
// major cap to major was recorded (0 before it is).
func (s *PostgresStore) improvedSinceMajorCap(ctx context.Context, f Family, c ActionClass,
	major int) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, improvedSinceMajorCapSQL, s.deployment, s.database,
		string(f), string(c), strconv.Itoa(major)).Scan(&n)
	return n, storeErr("count improved outcomes since the major cap", err)
}

// revalidate restamps st's major to current and records e, the cap's
// clearing by an admin, together; ErrConflict when the row moved on.
func (s *PostgresStore) revalidate(ctx context.Context, st State, current int,
	e Event) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		ok, err := s.stampMajor(ctx, tx, st, st.ServerMajor, current)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s/%s", ErrConflict, st.Family, st.Class)
		}
		return s.appendEvent(ctx, tx, e)
	})
}
