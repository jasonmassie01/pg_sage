package gameday

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The marker and the standbys of the target database.
const targetFactsSQL = `/* pg_sage gameday_target_facts v1 */
SELECT pg_catalog.shobj_description(d.oid, 'pg_database'),
       (SELECT count(*) FROM pg_catalog.pg_stat_replication)
  FROM pg_catalog.pg_database d
 WHERE d.datname = pg_catalog.current_database()`

// targetFacts is what Create reads on the target.
type targetFacts struct {
	id       ClusterIdentity
	marker   *string
	standbys int64
}

// readTarget reads the target over one short-lived, read-only connection.
func (p *LocalProvider) readTarget(ctx context.Context) (targetFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := p.connect(ctx, p.dsn)
	if err != nil {
		return targetFacts{}, fmt.Errorf("connect: %w", errors.Join(ctx.Err(), err))
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var f targetFacts
	if f.id, err = ReadClusterIdentity(ctx, conn); err != nil {
		return targetFacts{}, err
	}
	if err := conn.QueryRow(ctx, targetFactsSQL).Scan(&f.marker, &f.standbys); err != nil {
		return targetFacts{}, fmt.Errorf("read the database comment and standbys: %w",
			errors.Join(ctx.Err(), err))
	}
	return f, nil
}

// checkRefused refuses a target on the cluster of any refused database,
// and refuses when a refused database's identity is unreadable.
func (p *LocalProvider) checkRefused(ctx context.Context, target ClusterIdentity) error {
	refused, err := p.refused(ctx)
	if err != nil {
		return fmt.Errorf("%w: list the monitored databases: %w", ErrNotDisposable, err)
	}
	if len(refused) == 0 {
		return fmt.Errorf("%w: no monitored or control database to compare %s with",
			ErrNotDisposable, p.endpoint)
	}
	for _, q := range refused {
		id, err := readBounded(ctx, q)
		if err != nil {
			return fmt.Errorf("%w: cannot read the identity of %s to compare %s with: %w",
				ErrNotDisposable, describe(q), p.endpoint, err)
		}
		if sameCluster(target, id) {
			return fmt.Errorf("%w: %s is on the cluster of %s, and the fault programs act "+
				"on the whole cluster; point local_dsn at a separate cluster",
				ErrMonitoredDSN, p.endpoint, describe(q))
		}
	}
	return nil
}

func readBounded(ctx context.Context, q Querier) (ClusterIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return ReadClusterIdentity(ctx, q)
}

// checkDisposable refuses a standby, a primary with standbys and an
// unmarked database.
func (p *LocalProvider) checkDisposable(f targetFacts) error {
	switch {
	case f.id.InRecovery:
		return fmt.Errorf("%w: %s is a standby (in recovery); point local_dsn at a "+
			"disposable primary", ErrNotDisposable, p.endpoint)
	case f.standbys > 0:
		return fmt.Errorf("%w: %s streams to %d standbys; point local_dsn at a "+
			"disposable primary without standbys", ErrNotDisposable, p.endpoint, f.standbys)
	case f.marker == nil || *f.marker != TargetMarker:
		return fmt.Errorf("%w: %s is not marked disposable; if it is, run COMMENT ON "+
			"DATABASE %s IS '%s'", ErrNotDisposable, p.endpoint,
			pgx.Identifier{p.database}.Sanitize(), TargetMarker)
	}
	return nil
}

// describe names a refused database without its credentials.
func describe(q Querier) string {
	if pool, ok := q.(*pgxpool.Pool); ok && pool != nil {
		return endpointOfConfig(pool.Config().ConnConfig)
	}
	return "a monitored database"
}

// connectTarget opens the bounded, read-only probe connection.
func connectTarget(ctx context.Context, dsn string) (targetConn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("local_dsn is not a PostgreSQL DSN")
	}
	cfg.ConnectTimeout = probeTimeout
	for k, v := range map[string]string{"statement_timeout": "5s", "lock_timeout": "1s",
		"default_transaction_read_only": "on",
		"application_name":              "pg_sage_gameday_probe"} {
		cfg.RuntimeParams[k] = v
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
