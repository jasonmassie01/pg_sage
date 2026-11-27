package pressure

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// SQLOptions bound the SQL source's reads; zero is the default.
type SQLOptions struct {
	// InputTimeout is each input's statement_timeout (default 1 s); a read
	// that runs past it leaves only that input unknown.
	InputTimeout time.Duration
	// LockTimeout is each read's lock_timeout (default 100 ms).
	LockTimeout time.Duration
}

// SQLSource reads the inputs from the monitored database's catalog
// views, each in its own short read-only transaction.
type SQLSource struct {
	pool *pgxpool.Pool
	o    SQLOptions
}

// NewSQLSource reads pool's database.
func NewSQLSource(pool *pgxpool.Pool, o SQLOptions) *SQLSource {
	o.InputTimeout = orDuration(o.InputTimeout, time.Second)
	o.LockTimeout = orDuration(o.LockTimeout, 100*time.Millisecond)
	return &SQLSource{pool: pool, o: o}
}

// Read resolves table through to_regclass (never interpolated) and reads
// every input; an input that fails is Unknown with the reason.
func (s *SQLSource) Read(ctx context.Context, table string) (Readings, error) {
	if s == nil || s.pool == nil {
		return Readings{}, ErrUnavailable
	}
	relid, err := s.resolve(ctx, table)
	if err != nil {
		return Readings{}, err
	}
	lag, replay := s.replicaLag(ctx)
	r := Readings{Inputs: map[string]Reading{policy.PressureReplicaLag: lag},
		ReplayPosition: replay}
	xid, mxid := s.freezeAges(ctx, &r)
	r.Inputs[policy.PressureXIDAge], r.Inputs[policy.PressureMultixactAge] = xid, mxid
	if relid == 0 {
		none := NotApplicable("no target table named")
		r.Inputs[policy.PressureVacuum], r.Inputs[policy.PressureLockQueue] = none, none
	} else {
		r.Inputs[policy.PressureVacuum] = s.count(ctx, vacuumSQL, relid)
		r.Inputs[policy.PressureLockQueue] = s.count(ctx, lockQueueSQL, relid)
	}
	r.WALPosition = s.WALPosition(ctx)
	return r, nil
}

// readTx runs fn in a read-only transaction with the input's timeouts.
func (s *SQLSource) readTx(ctx context.Context, fn func(pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.o.InputTimeout+500*time.Millisecond)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true)`, ms(s.o.InputTimeout),
		ms(s.o.LockTimeout)); err != nil {
		return err
	}
	return fn(tx)
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + "ms" }

// resolve is table's oid (0 for no table). A name to_regclass does not
// resolve, or that is not a valid name at all (PG14 raises what PG16+
// returns NULL for), is ErrUnknownTable.
func (s *SQLSource) resolve(ctx context.Context, table string) (uint32, error) {
	if table == "" {
		return 0, nil
	}
	var oid *uint32
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT to_regclass($1)::oid`, table).Scan(&oid)
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && unknownNameCodes[pgErr.Code]:
		return 0, fmt.Errorf("%w: %q is not a valid table name", ErrUnknownTable, table)
	case err != nil:
		return 0, fmt.Errorf("resolve table %q: %w", table, err)
	case oid == nil:
		return 0, fmt.Errorf("%w: no table %q", ErrUnknownTable, table)
	}
	return *oid, nil
}

// unknownNameCodes are the SQLSTATEs to_regclass raises for a name that
// cannot name a table here: syntax error, invalid name, a cross-database
// reference, an unknown schema.
var unknownNameCodes = map[string]bool{"42601": true, "42602": true, "0A000": true,
	"3F000": true}

// replicaLag is the worst replay lag of the walsenders (0 for a replica
// that has replayed the current position, not applicable without any)
// and the slowest replica's replay position in bytes. A walsender whose
// state is hidden (no pg_monitor) makes the lag unknown.
func (s *SQLSource) replicaLag(ctx context.Context) (Reading, Reading) {
	var replicas, hidden, unmeasured int
	var lag float64
	var replay *float64
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, replicaLagSQL).Scan(&replicas, &hidden, &unmeasured, &lag,
			&replay)
	})
	none := NotApplicable("no replica behind")
	switch {
	case err != nil:
		return Unknown(inputFailure(err, s.o.InputTimeout)), none
	case replicas == 0:
		return NotApplicable("no replicas"), none
	case hidden > 0:
		return Unknown("pg_stat_replication hides replica state from this role; grant " +
			"pg_monitor"), none
	case unmeasured > 0:
		return Unknown(strconv.Itoa(unmeasured) + " replica(s) behind with no replay " +
			"lag measured yet"), none
	}
	now := time.Now()
	if replay != nil {
		none = Known(*replay, now)
	}
	return Known(lag, now), none
}

// replicaLagSQL reads the streaming walsenders (a base backup's is not a
// replica). A receiver that never replays (pg_receivewal) is measured by
// its flush position and lag instead.
const replicaLagSQL = `/* pg_sage */ SELECT count(*)::int,
	count(*) FILTER (WHERE state IS NULL)::int,
	count(*) FILTER (WHERE state IS NOT NULL AND NOT caught_up AND lag IS NULL)::int,
	coalesce(max(CASE WHEN caught_up THEN 0 ELSE extract(epoch FROM lag) END), 0)::float8,
	min(pg_wal_lsn_diff(position, '0/0')) FILTER (WHERE NOT caught_up)::float8
	FROM (SELECT state, coalesce(replay_lag, flush_lag) AS lag,
		coalesce(replay_lsn, flush_lsn) AS position,
		coalesce(coalesce(replay_lsn, flush_lsn) >= CASE WHEN pg_is_in_recovery()
			THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END, false) AS caught_up
		FROM pg_stat_replication
		WHERE state IS NULL OR state IN ('streaming', 'catchup')) r`

// freezeAges are the cluster's oldest transaction and multixact ID ages
// (wraparound is cluster-wide), recording the freeze maxima in r.
func (s *SQLSource) freezeAges(ctx context.Context, r *Readings) (Reading, Reading) {
	var xid, mxid, xidMax, mxidMax float64
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT max(age(datfrozenxid))::float8,
			max(mxid_age(datminmxid))::float8,
			current_setting('autovacuum_freeze_max_age')::float8,
			current_setting('autovacuum_multixact_freeze_max_age')::float8
			FROM pg_database`).Scan(&xid, &mxid, &xidMax, &mxidMax)
	})
	if err != nil {
		reason := inputFailure(err, s.o.InputTimeout)
		return Unknown(reason), Unknown(reason)
	}
	r.XIDFreezeMax, r.MXIDFreezeMax = xidMax, mxidMax
	now := time.Now()
	return Known(xid, now), Known(mxid, now)
}

// vacuumSQL counts vacuums running on the table in this database; a row
// whose relation is hidden (no pg_read_all_stats) makes it unknown.
const vacuumSQL = `/* pg_sage */ SELECT count(*) FILTER (WHERE relid = $1::oid)::int,
	count(*) FILTER (WHERE relid IS NULL)::int
	FROM pg_stat_progress_vacuum
	WHERE datid = (SELECT oid FROM pg_database WHERE datname = current_database())`

// lockQueueSQL counts backends waiting for a lock on the table.
const lockQueueSQL = `/* pg_sage */ SELECT count(*)::int, 0
	FROM pg_locks WHERE locktype = 'relation' AND NOT granted AND relation = $1::oid
	AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`

func (s *SQLSource) count(ctx context.Context, sql string, relid uint32) Reading {
	var n, hidden int
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, relid).Scan(&n, &hidden)
	})
	switch {
	case err != nil:
		return Unknown(inputFailure(err, s.o.InputTimeout))
	case hidden > 0:
		return Unknown("pg_stat_progress_vacuum hides other roles' vacuums; grant " +
			"pg_read_all_stats")
	}
	return Known(float64(n), time.Now())
}

// WALPosition is the WAL insert position in bytes; unknown on a standby.
func (s *SQLSource) WALPosition(ctx context.Context) Reading {
	if s == nil || s.pool == nil {
		return Unknown("no connection pool")
	}
	var pos *float64
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT CASE WHEN pg_is_in_recovery()
			THEN NULL ELSE pg_wal_lsn_diff(pg_current_wal_insert_lsn(), '0/0') END::float8`).
			Scan(&pos)
	})
	switch {
	case err != nil:
		return Unknown(inputFailure(err, s.o.InputTimeout))
	case pos == nil:
		return Unknown("the server is a standby: it writes no WAL of its own")
	}
	return Known(*pos, time.Now())
}

// inputFailure is why an input could not be read, for its reason.
func inputFailure(err error, timeout time.Duration) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &pgErr) && pgErr.Code == "57014":
		return "timed out after " + timeout.String()
	case errors.As(err, &pgErr) && pgErr.Code == "42501":
		return "permission denied: " + pgErr.Message
	}
	return err.Error()
}
