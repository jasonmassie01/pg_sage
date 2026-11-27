package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrency test beyond PoolExhaust: internalError is stateless.

func serveInternalError(err error) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	internalError(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil), "probe", err)
	return rec
}

func realConnectError(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pgconn.Connect(ctx, unreachableDSN)
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("connect to a closed port: %T %v, want *pgconn.ConnectError", err, err)
	}
	return err
}

func TestInternalError_DBUnavailableIs503WithRetryAfter(t *testing.T) {
	cases := map[string]error{
		"connect error":         fmt.Errorf("list users: %w", realConnectError(t)),
		"pool acquire deadline": fmt.Errorf("list users: %w", context.DeadlineExceeded),
	}
	for name, err := range cases {
		rec := serveInternalError(err)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusServiceUnavailable || body["code"] != "db_unavailable" ||
			rec.Header().Get("Retry-After") != dbUnavailableRetryAfter {
			t.Errorf("%s: %d %v Retry-After %q", name, rec.Code, body,
				rec.Header().Get("Retry-After"))
		}
		if strings.Contains(rec.Body.String(), "127.0.0.1") {
			t.Errorf("%s: 503 body leaks the connection target: %s", name, rec.Body.String())
		}
	}
}

func TestInternalError_OtherErrorsKeepThe500Body(t *testing.T) {
	cases := map[string]error{
		"plain":    errors.New("boom"),
		"canceled": context.Canceled,
		"http deadline": &url.Error{Op: "Post", URL: "https://llm.test",
			Err: context.DeadlineExceeded},
		"postgres error": &pgconn.PgError{Code: "42P01", Message: "relation missing"},
		"wrapped error":  fmt.Errorf("op: %w", errors.New("x")),
	}
	for name, err := range cases {
		rec := serveInternalError(err)
		if rec.Code != http.StatusInternalServerError ||
			strings.TrimSpace(rec.Body.String()) != `{"error":"internal error"}` ||
			rec.Header().Get("Retry-After") != "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if dbUnavailable(nil) {
		t.Error("nil error classified as db unavailable")
	}
}

// exhaustedPool opens a MaxConns=1 pool on the test database and holds its
// only connection for the test.
func exhaustedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	surfacePool(t) // skips without a database, bootstraps the schema
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the only connection: %v", err)
	}
	t.Cleanup(held.Release)
	return pool
}

// DB: a real handler on a pool whose only connection is held elsewhere
// answers 503 db_unavailable, not 500.
func TestPoolExhaust_HandlerAnswers503DBUnavailable(t *testing.T) {
	pool := exhaustedPool(t)
	h := newTestRouter(nil, config.DefaultConfig(), pool, nil, nil, nil, nil,
		fakeAdminMiddleware)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/tokens", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusServiceUnavailable || body["code"] != "db_unavailable" ||
		rec.Header().Get("Retry-After") == "" {
		t.Fatalf("exhausted pool: %d %s Retry-After %q", rec.Code, rec.Body.String(),
			rec.Header().Get("Retry-After"))
	}
}

// DB: a statement that times out on a connection it holds is not a pool
// problem and stays a 500.
func TestPoolExhaust_QueryTimeoutStays500(t *testing.T) {
	pool := surfacePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	_, err = conn.Exec(ctx, "SELECT pg_sleep(5)")
	if err == nil {
		t.Fatal("pg_sleep(5) finished inside 100ms")
	}
	if rec := serveInternalError(err); rec.Code != http.StatusInternalServerError {
		t.Fatalf("query timeout (%v) = %d, want 500", err, rec.Code)
	}
}

// DB: with the real session middleware, a viewer's refused POST is logged
// with the viewer's id and role.
func TestAccessLog_RealSessionViewerForbiddenPost(t *testing.T) {
	pool := surfacePool(t)
	f := surfaceRouter(t, pool, config.DefaultConfig(), nil)
	email := f.login(t, auth.RoleViewer)
	var id int
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM sage.users WHERE email=$1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	buf := captureAccessLog(t)
	if status, body := f.request(t, http.MethodPost, "/api/v1/emergency-stop",
		`{"reason":"body-secret"}`); status != http.StatusForbidden {
		t.Fatalf("viewer POST = %d %s, want 403", status, body)
	}
	lines := accessLines(buf)
	if len(lines) != 1 || !strings.Contains(lines[0], "user_id="+strconv.Itoa(id)) ||
		!strings.Contains(lines[0], "role=viewer") ||
		strings.Contains(buf.String(), "body-secret") {
		t.Fatalf("access lines = %v", lines)
	}
}
