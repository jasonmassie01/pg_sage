package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A rehearsal runs statements on a clone that holds production data, so
// before the first one the rehearser proves the clone is not the
// production server (migration-07, lab-04a). A clone on the same server as
// production, under any DSN or database name, is refused: the same server
// answers with the same postmaster start time on the same port.
// system_identifier is not compared, because a Database Lab clone shares
// it with its source.

// Identity errors: a refused target, and a target that could not be shown
// not to be production. Both mean nothing ran on it.
var (
	ErrTargetIsProduction = errors.New("rehearsal target is the production server")
	ErrTargetUnverified   = errors.New("rehearsal target not verified as non-production")
)

// ServerIdentity names one PostgreSQL server: where a DSN points and which
// postmaster answered on it.
type ServerIdentity struct {
	Host      string    // DSN host, lower-cased
	Port      uint16    // DSN port
	StartedAt time.Time // pg_postmaster_start_time()
	// ServerPort is inet_server_port(); 0 over a Unix socket.
	ServerPort int32
}

// IdentitySource reads the production server's identity when a
// rehearsal starts (a restart changes it).
type IdentitySource func(context.Context) (ServerIdentity, error)

// identityTimeout bounds one identity probe.
const identityTimeout = 5 * time.Second

// PoolIdentity is the identity of the server pool connects to: the
// monitored (production) database.
func PoolIdentity(pool *pgxpool.Pool) IdentitySource {
	return func(ctx context.Context) (ServerIdentity, error) {
		if pool == nil {
			return ServerIdentity{}, errors.New("production pool is unavailable")
		}
		return readServerIdentity(ctx, pool)
	}
}

const serverIdentitySQL = `/* pg_sage rehearsal_identity v1 */
SELECT pg_postmaster_start_time(), COALESCE(inet_server_port(), 0)`

// readServerIdentity is a read-only, time-bounded probe.
func readServerIdentity(ctx context.Context, pool *pgxpool.Pool) (ServerIdentity, error) {
	conn := pool.Config().ConnConfig
	id := ServerIdentity{Host: strings.ToLower(conn.Host), Port: conn.Port}
	ctx, cancel := context.WithTimeout(ctx, identityTimeout)
	defer cancel()
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 5000;
				SET LOCAL lock_timeout = 1000`); err != nil {
				return err
			}
			return tx.QueryRow(ctx, serverIdentitySQL).Scan(&id.StartedAt, &id.ServerPort)
		})
	if err != nil {
		return ServerIdentity{}, fmt.Errorf("read server identity of %s:%d: %w", id.Host,
			id.Port, err)
	}
	return id, nil
}

// sameServer reports whether clone is production's server: the same DSN
// host and port, or the same postmaster on the same port. A server port of
// 0 (a Unix-socket connection) is unknown, not different: the same
// postmaster start time with an unknown port is refused (fail closed).
func sameServer(production, clone ServerIdentity) bool {
	if production.Host != "" && production.Host == clone.Host &&
		production.Port == clone.Port {
		return true
	}
	if production.StartedAt.IsZero() || !production.StartedAt.Equal(clone.StartedAt) {
		return false
	}
	return production.ServerPort == clone.ServerPort || production.ServerPort == 0 ||
		clone.ServerPort == 0
}

// RefuseProduction is ErrTargetIsProduction when clone is production's
// server, else nil. The error names hosts and ports, never credentials.
func RefuseProduction(production, clone ServerIdentity) error {
	if !sameServer(production, clone) {
		return nil
	}
	return fmt.Errorf("%w: clone %s:%d is production %s:%d (postmaster started %s); "+
		"nothing ran on it", ErrTargetIsProduction, clone.Host, clone.Port,
		production.Host, production.Port, production.StartedAt.UTC().Format(time.RFC3339Nano))
}

// ProductionIdentity reads source and fails closed: without a production
// identity a clone cannot be shown not to be production.
func ProductionIdentity(ctx context.Context, source IdentitySource) (ServerIdentity, error) {
	if source == nil {
		return ServerIdentity{}, fmt.Errorf("%w: no production identity source",
			ErrTargetUnverified)
	}
	id, err := source(ctx)
	if err != nil {
		return ServerIdentity{}, fmt.Errorf("%w: %w", ErrTargetUnverified, err)
	}
	if id.StartedAt.IsZero() {
		return ServerIdentity{}, fmt.Errorf("%w: production postmaster start time unknown",
			ErrTargetUnverified)
	}
	return id, nil
}

// GuardedConnect connects to cl only after its DSN, then the server that
// answers on it, proved not to be production. The caller closes the pool.
func GuardedConnect(ctx context.Context, cl Clone, production IdentitySource,
) (*pgxpool.Pool, error) {
	prod, err := ProductionIdentity(ctx, production)
	if err != nil {
		return nil, err
	}
	if cl.DSN == "" {
		return nil, fmt.Errorf("%w: clone %s has no DSN", ErrTargetUnverified, cl.ID)
	}
	parsed, err := pgxpool.ParseConfig(cl.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse rehearsal clone DSN: %w", err)
	}
	target := ServerIdentity{Host: strings.ToLower(parsed.ConnConfig.Host),
		Port: parsed.ConnConfig.Port}
	if err := RefuseProduction(prod, target); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("connect rehearsal clone: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping rehearsal clone: %w", err)
	}
	if target, err = readServerIdentity(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w: %w", ErrTargetUnverified, err)
	}
	if err := RefuseProduction(prod, target); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
