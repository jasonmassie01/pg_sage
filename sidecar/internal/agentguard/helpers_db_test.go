package agentguard

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentguard"))
}

// livePool is a superuser pool on the package's fixture database, with
// the sage schema bootstrapped.
func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, schema.Bootstrap(ctx, pool))
	return pool
}

var uniqSeq atomic.Int64

// uniqName is a principal slug unique within the run.
func uniqName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1e9, uniqSeq.Add(1))
}

func createUser(t *testing.T, pool *pgxpool.Pool, role string) int {
	t.Helper()
	email := uniqName("guard-"+role) + "@example.com"
	id, err := auth.CreateUser(context.Background(), pool, email, "password-123", role)
	require.NoError(t, err)
	return id
}

func newPrincipal(t *testing.T, s *Store, sponsor *int) Principal {
	t.Helper()
	p, err := s.Create(context.Background(), CreateRequest{Name: uniqName("bot"),
		SponsorUserID: sponsor, Profile: "readonly-analyst", EnvCeiling: EnvProd,
		CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	return p
}

func testKeyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	kr, err := crypto.NewKeyring(key)
	require.NoError(t, err)
	return kr
}

// serverVersionNum reads the fixture server's version.
func serverVersionNum(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	v, err := serverVersion(context.Background(), pool)
	require.NoError(t, err)
	return v
}

// withUser returns dsn with its user and password replaced.
func withUser(t *testing.T, dsn, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(user, password)
	return u.String()
}

// currentDatabase names the fixture database.
func currentDatabase(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var db string
	require.NoError(t, pool.QueryRow(context.Background(),
		"SELECT current_database()").Scan(&db))
	return db
}

// tryLogin connects as user and returns current_user, or the error.
func tryLogin(ctx context.Context, dsn string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cctx, dsn)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var who string
	err = conn.QueryRow(cctx, "SELECT current_user::text").Scan(&who)
	return who, err
}

// tryShow connects with dsn and reads one setting.
func tryShow(ctx context.Context, dsn, setting string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cctx, dsn)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var v string
	err = conn.QueryRow(cctx, "SELECT current_setting($1)", setting).Scan(&v)
	return v, err
}
