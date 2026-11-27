// Package clusterlock holds named locks across every database of one
// PostgreSQL cluster and across processes: session-scoped advisory locks
// taken in the cluster's maintenance database. A lock ends with its
// connection, so a holder that crashes releases it.
package clusterlock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaintenanceDatabase is where the locks are taken. Advisory locks are
// per database, so one database every client can reach makes them
// cluster-wide.
const MaintenanceDatabase = "postgres"

// keyPrefix namespaces the lock names (hashtext of prefix + name).
const keyPrefix = "pg_sage_cluster_fixture:"

// Key is the text whose hashtext is the advisory lock key of the cluster
// lock name, for finding the lock's holders and waiters in pg_locks.
func Key(name string) string { return keyPrefix + name }

// closeTimeout bounds closing a lock's connection.
const closeTimeout = 5 * time.Second

// Lock waits until it holds the cluster lock name on dsn's server, or ctx
// ends. release ends the session and with it the lock.
func Lock(ctx context.Context, dsn, name string) (func(), error) {
	conn, err := connect(ctx, dsn, name)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))",
		Key(name)); err != nil {
		closeConn(conn)
		return nil, fmt.Errorf("cluster lock %q: %w", name, err)
	}
	return releaser(conn), nil
}

// LockConfig is Lock for a connection configuration (a pool's, with its
// authentication hooks), not a DSN: the lock's session copies cfg and
// connects to the maintenance database.
func LockConfig(ctx context.Context, cfg *pgx.ConnConfig, name string) (func(), error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("cluster lock: empty lock name")
	}
	if cfg == nil {
		return nil, errors.New("cluster lock: no connection configuration")
	}
	c := cfg.Copy()
	c.Database = MaintenanceDatabase
	c.RuntimeParams["application_name"] = "pg_sage cluster lock"
	conn, err := pgx.ConnectConfig(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("cluster lock %q: connect maintenance database %s on %s:%d: %w",
			name, MaintenanceDatabase, c.Host, c.Port, err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))",
		Key(name)); err != nil {
		closeConn(conn)
		return nil, fmt.Errorf("cluster lock %q: %w", name, err)
	}
	return releaser(conn), nil
}

// TryLock takes the cluster lock name without waiting: ok is false (and
// err nil) when another session holds it.
func TryLock(ctx context.Context, dsn, name string) (release func(), ok bool, err error) {
	conn, err := connect(ctx, dsn, name)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))",
		Key(name)).Scan(&got); err != nil {
		closeConn(conn)
		return nil, false, fmt.Errorf("cluster lock %q: %w", name, err)
	}
	if !got {
		closeConn(conn)
		return nil, false, nil
	}
	return releaser(conn), true, nil
}

// connect opens the lock's session in the maintenance database of dsn's
// server. Errors never carry the DSN (it may hold a password).
func connect(ctx context.Context, dsn, name string) (*pgx.Conn, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("cluster lock: empty lock name")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("cluster lock: parse dsn: not a PostgreSQL DSN")
	}
	cfg.Database = MaintenanceDatabase
	cfg.RuntimeParams["application_name"] = "pg_sage cluster lock"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster lock %q: connect maintenance database %s on %s:%d: %w",
			name, MaintenanceDatabase, cfg.Host, cfg.Port, err)
	}
	return conn, nil
}

func releaser(conn *pgx.Conn) func() {
	var once sync.Once
	return func() { once.Do(func() { closeConn(conn) }) }
}

func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = conn.Close(ctx)
}
