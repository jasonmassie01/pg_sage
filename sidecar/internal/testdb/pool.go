package testdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SharedPoolMaxConns caps each package-wide pool (ci-16). A test that needs
// more connections at once, or connection-level state of its own, opens
// its own pool.
const SharedPoolMaxConns = 8

// Opening and closing a pool per test leaves every closed client socket in
// TIME_WAIT (up to 120 s on Windows), and a full round of packages running
// at once can use up the host's dynamic port range. Pool and PoolFor hand
// every test of a package process the same pool per DSN instead. Run and
// RunBootstrapped open the registry before the tests and close every pool
// after them, before dropping the fixture database.
var (
	errNoDSN = errors.New("no DSN: pass the test database URL " +
		"(testdb.SkipUnlessLive), not an empty string")
	errPoolsNotOpen = errors.New("shared pools are not open: the package's TestMain " +
		"must call testdb.Run or testdb.RunBootstrapped, which closes them")
	errPoolsClosed = errors.New("shared pools are closed: the package's tests " +
		"have finished (a goroutine outlived its test?)")
)

type registryState int

const (
	registryIdle registryState = iota
	registryOpen
	registryClosed
)

// poolRegistry holds one pool per DSN while a package's tests run.
type poolRegistry struct {
	mu    sync.Mutex
	state registryState
	pools map[string]*pgxpool.Pool
}

var sharedPools = &poolRegistry{}

// Pool returns the package-wide pool on the package's fixture database,
// skipping t when no test server is configured. Tests share it, so a test
// must not close it or leave session state on its connections (SET, SET
// ROLE, temporary tables, advisory locks, HypoPG hypothetical indexes):
// tests that assert connection-level behaviour keep their own pool.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return PoolFor(t, SkipUnlessLive(t))
}

// PoolFor returns the package-wide pool for dsn, failing t when dsn is
// empty or malformed or when the package's TestMain does not route
// through Run. dsn must outlive the package's tests (the fixture or the
// server's own database): a database a test creates and drops keeps a
// pool of its own. Using the pool after Run closed it returns pgxpool's
// "closed pool" error.
func PoolFor(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := sharedPools.get(dsn)
	if err != nil {
		t.Fatalf("testdb: shared pool: %v", err)
	}
	return pool
}

// open starts a package run: pools may be created until close.
func (r *poolRegistry) open() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = registryOpen
	r.pools = map[string]*pgxpool.Pool{}
}

// get returns the pool for dsn, creating it on first use. Creating a pool
// does not connect: connections open on first use, at most MaxConns.
func (r *poolRegistry) get(dsn string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errNoDSN
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse the shared pool's DSN: %w", err)
	}
	if !strings.Contains(dsn, "pool_max_conns") {
		cfg.MaxConns = SharedPoolMaxConns
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case registryIdle:
		return nil, errPoolsNotOpen
	case registryClosed:
		return nil, errPoolsClosed
	}
	if pool, ok := r.pools[dsn]; ok {
		return pool, nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("create the shared pool: %w", err)
	}
	r.pools[dsn] = pool
	return pool, nil
}

// close ends a package run: it closes every pool, and get fails from now on.
func (r *poolRegistry) close() {
	r.mu.Lock()
	pools := r.pools
	r.pools = nil
	r.state = registryClosed
	r.mu.Unlock()
	for _, pool := range pools {
		pool.Close()
	}
}
