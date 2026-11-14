package selfmonitor

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

var errInjectedWrite = errors.New("injected write failure")

// failingPinConn is a fake connection: it passes every write to the server
// and records it, except a write that carries marker, which closes the
// connection and fails, as a server that dropped the session would.
type failingPinConn struct {
	net.Conn
	marker []byte
	mu     *sync.Mutex
	writes *[]string
}

func (c *failingPinConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	*c.writes = append(*c.writes, string(p))
	c.mu.Unlock()
	if bytes.Contains(p, c.marker) {
		_ = c.Conn.Close()
		return 0, errInjectedWrite
	}
	return c.Conn.Write(p)
}

// Error propagation: the search_path pin failing fails the connection with
// an error naming search_path, the pool reports it on acquire, and the
// READ COMMITTED pin was sent before it (chaining order).
func TestConfigurePool_SearchPathPinFailureFailsAcquire(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var writes []string
	cfg.ConnConfig.AfterNetConnect = func(
		_ context.Context, _ *pgconn.Config, conn net.Conn,
	) (net.Conn, error) {
		return &failingPinConn{Conn: conn, marker: []byte("search_path = pg_catalog"),
			mu: &mu, writes: &writes}, nil
	}
	ConfigurePool(cfg)
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pool.Acquire(ctx)
	if err == nil {
		conn.Release()
		t.Fatal("acquire succeeded although the search_path pin could not be applied")
	}
	if !strings.Contains(err.Error(), "search_path") {
		t.Fatalf("acquire error = %v, want it to name search_path", err)
	}
	mu.Lock()
	defer mu.Unlock()
	isolation, path := -1, -1
	for i, w := range writes {
		if isolation < 0 && strings.Contains(w, "default_transaction_isolation") {
			isolation = i
		}
		if path < 0 && strings.Contains(w, "search_path = pg_catalog") {
			path = i
		}
	}
	if isolation < 0 || path < 0 || isolation > path {
		t.Fatalf("isolation pin at write %d, search_path pin at %d: want both, "+
			"isolation first", isolation, path)
	}
}

// Error propagation on a single connection: the pin's own error names
// search_path and wraps the cause.
func TestPinSearchPath_FailureNamesSearchPath(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pc, err := pgconn.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		return pc.Close(ctx) // the pin then writes to a closed connection
	}
	pinSearchPath(cfg.ConnConfig)
	err = cfg.ConnConfig.AfterConnect(ctx, pc)
	if err == nil || !strings.Contains(err.Error(), "pin search_path to pg_catalog, pg_temp") {
		t.Fatalf("err = %v, want it to name the search_path pin", err)
	}
}
