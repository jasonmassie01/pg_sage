package agentposture

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentposture"))
}

// livePool connects to the package's fixture database.
func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func execAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// suffix is a short random lower-case suffix for cluster-wide names
// (roles are shared by every package's fixture database).
func suffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return strings.ToLower(base32.StdEncoding.EncodeToString(b))[:8]
}

// registeredRoleName is a role name in the Guard naming scheme (§6.6):
// sage_agentb_ plus 10 lower base32 characters.
func registeredRoleName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return "sage_agentb_" + strings.ToLower(base32.StdEncoding.EncodeToString(b))[:10]
}

// createRole creates a cluster role (attrs appended to CREATE ROLE) and
// drops it, with everything it owns in this database, when the test ends.
func createRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name,
	attrs string) {
	t.Helper()
	execAll(t, ctx, pool, fmt.Sprintf("CREATE ROLE %s %s", pgx.Identifier{name}.Sanitize(),
		attrs))
	t.Cleanup(func() { dropRole(t, pool, name) })
}

func dropRole(t *testing.T, pool *pgxpool.Pool, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := pgx.Identifier{name}.Sanitize()
	for _, s := range []string{"DROP OWNED BY " + id, "DROP ROLE IF EXISTS " + id} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Errorf("cleanup %s: %v", s, err)
			return
		}
	}
}

// loginAs opens a session as role (which needs LOGIN and the password)
// with the given application_name, closed when the test ends.
func loginAs(t *testing.T, ctx context.Context, role, secret, app string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.User, cfg.Password = role, secret
	cfg.RuntimeParams["application_name"] = app
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// readTx runs fn in a read-only transaction on pool, the way the first
// look and the monitor run detectors.
func readTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	fn func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	fn(tx)
}

// roleNames lists the names of rs.
func roleNames(rs []Role) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

func findRole(rs []Role, name string) (Role, bool) {
	for _, r := range rs {
		if r.Name == name {
			return r, true
		}
	}
	return Role{}, false
}

type pgxTx = pgx.Tx
