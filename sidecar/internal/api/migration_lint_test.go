package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/migration"
)

// lintCatalog answers Review's reads: rows for the table, nothing else
// active. fail makes the table-stats read fail; inFlight tracks the most
// reads running at once.
type lintCatalog struct {
	rows      int64
	fail      error
	delay     time.Duration
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func (c *lintCatalog) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("lintCatalog: Query not supported")
}

func (c *lintCatalog) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	n := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for m := c.maxFlight.Load(); n > m && !c.maxFlight.CompareAndSwap(m, n); {
		m = c.maxFlight.Load()
	}
	time.Sleep(c.delay)
	return lintRow{c: c, sql: sql}
}

type lintRow struct {
	c   *lintCatalog
	sql string
}

func (r lintRow) Scan(dest ...any) error {
	switch {
	case strings.Contains(r.sql, "server_version_num"):
		*(dest[0].(*int)) = 170000
	case strings.Contains(r.sql, "reltuples"):
		if r.c.fail != nil {
			return r.c.fail
		}
		*(dest[0].(*int64)), *(dest[1].(*int64)) = r.c.rows, r.c.rows*100
	case strings.Contains(r.sql, "pg_locks"):
		*(dest[0].(*int)) = 0
	case strings.Contains(r.sql, "pg_stat_activity"):
		*(dest[0].(*int)), *(dest[1].(*float64)) = 0, 0
	case strings.Contains(r.sql, "pg_stat_replication"):
		*(dest[0].(*float64)) = 0
	default:
		*(dest[0].(*bool)) = false // pg_available_extensions
	}
	return nil
}

func lintDeps(c *lintCatalog, limit int) migrationLintDeps {
	return migrationLintDeps{
		querier: func(db string) (catalogread.Querier, error) {
			if db == "missing" {
				return nil, errLintUnknownDatabase
			}
			return c, nil
		},
		limiter: newLintLimiter(limit, time.Minute),
		slots:   make(chan struct{}, migrationLintSlots)}
}

func postLint(t *testing.T, h http.Handler, user *auth.User,
	body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, migrationLintPath, strings.NewReader(body))
	if user != nil {
		req = withUser(req, user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func lintBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// Happy path: a plain CREATE INDEX on a 1M-row table is risky with the
// CONCURRENTLY rewrite, for a viewer.
func TestMigrationLintRisky(t *testing.T) {
	h := migrationLintHandler(lintDeps(&lintCatalog{rows: 1_000_000}, 30))
	rec := postLint(t, h, testViewerUser(), `{"sql":"CREATE INDEX x ON app.t (c)"}`)
	var rep migration.Report
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &rep) != nil {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if rep.Verdict != "risky" || rep.Static || !strings.Contains(
		rep.Statements[0].GuardedRewrite, "CREATE INDEX CONCURRENTLY x") ||
		rep.Statements[0].Hazards[0].LiveRows != 1_000_000 {
		t.Fatalf("report %+v", rep)
	}
	rec = postLint(t, h, testViewerUser(), `{"sql":"CREATE INDEX x ON app.t (c)",
		"static":true,"pg_version":16}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"verdict":"review"`) ||
		!strings.Contains(rec.Body.String(), `"static":true`) {
		t.Fatalf("static: %d %s", rec.Code, rec.Body)
	}
}

// Invalid input is 400 with a reason; an unknown database is 404.
func TestMigrationLintInvalidInput(t *testing.T) {
	h := migrationLintHandler(lintDeps(&lintCatalog{}, 1000))
	over := strings.Repeat("-", migration.MaxReviewBytes+1)
	huge := strings.Repeat("x", migrationLintMaxBody+1)
	cases := map[string]struct {
		body string
		code int
		want string
	}{
		"empty sql":     {`{"sql":"  "}`, 400, "empty"},
		"over 100 kB":   {lintBody(t, map[string]string{"sql": over}), 400, "100001 bytes"},
		"body too big":  {`{"sql":"` + huge + `"}`, 400, "over"},
		"not JSON":      {`CREATE INDEX x ON t (c)`, 400, "JSON"},
		"unknown field": {`{"sql":"SELECT 1","run":true}`, 400, "JSON"},
		"two objects":   {`{"sql":"SELECT 1"}{"sql":"SELECT 2"}`, 400, "one JSON object"},
		"bad database":  {`{"sql":"SELECT 1","database":"a b"}`, 400, "database"},
		"bad version":   {`{"sql":"SELECT 1","static":true,"pg_version":12}`, 400, "pg_version"},
		"unknown db":    {`{"sql":"SELECT 1","database":"missing"}`, 404, "not found"},
	}
	for name, tc := range cases {
		rec := postLint(t, h, testViewerUser(), tc.body)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s, want %d containing %q", name, rec.Code, rec.Body, tc.code,
				tc.want)
		}
	}
	rec := postLint(t, h, testViewerUser(), `{"sql":"CREATE FROBNICATE x"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"verdict":"review"`) {
		t.Fatalf("unparseable SQL: %d %s, want review", rec.Code, rec.Body)
	}
}

// Error propagation: a catalog timeout is 503 with the reason, never a
// verdict; no connected database is 503.
func TestMigrationLintCatalogTimeoutIs503(t *testing.T) {
	c := &lintCatalog{fail: errors.New("canceling statement due to statement timeout")}
	h := migrationLintHandler(lintDeps(c, 30))
	rec := postLint(t, h, testViewerUser(), `{"sql":"CREATE INDEX x ON app.t (c)"}`)
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "statement timeout") ||
		strings.Contains(rec.Body.String(), "verdict") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if q, err := fleetLintQuerier(nil)(""); q != nil || !errors.Is(err, errLintNoDatabase) ||
		lintDatabaseStatus(err) != http.StatusServiceUnavailable {
		t.Fatalf("no fleet: %v %v", q, err)
	}
}

// Boundary: the 31st request in a minute is 429 with Retry-After; another
// user has their own budget; the window slides.
func TestMigrationLintRateLimit(t *testing.T) {
	h := migrationLintHandler(lintDeps(&lintCatalog{}, 30))
	body := `{"sql":"SELECT 1","static":true}`
	for i := 0; i < 30; i++ {
		if rec := postLint(t, h, testViewerUser(), body); rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := postLint(t, h, testViewerUser(), body)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("31st request: %d %v", rec.Code, rec.Header())
	}
	if rec := postLint(t, h, testAdminUser(), body); rec.Code != 200 {
		t.Fatalf("another user was limited: %d", rec.Code)
	}
	l := newLintLimiter(2, time.Minute)
	t0 := time.Unix(1000, 0)
	_, a := l.allow("k", t0)
	_, b := l.allow("k", t0.Add(time.Second))
	wait, c := l.allow("k", t0.Add(2*time.Second))
	_, d := l.allow("k", t0.Add(time.Minute))
	if !a || !b || c || wait != 58*time.Second || !d {
		t.Fatalf("window: %v %v %v (%s) %v", a, b, c, wait, d)
	}
}

// Concurrency: 50 parallel requests from one user stay within the rate
// limit (30) and the slot cap. (-race is unavailable here: no cgo.)
func TestMigrationLintConcurrentRequests(t *testing.T) {
	c := &lintCatalog{rows: 10, delay: 2 * time.Millisecond}
	h := migrationLintHandler(lintDeps(c, 30))
	var ok, limited atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch postLint(t, h, testViewerUser(), `{"sql":"CREATE INDEX x ON app.t (c)"}`).Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 30 || limited.Load() != 20 {
		t.Fatalf("%d ok, %d limited; want 30 and 20", ok.Load(), limited.Load())
	}
	if m := c.maxFlight.Load(); m < 1 || m > migrationLintSlots {
		t.Fatalf("%d catalog reads at once, want 1..%d", m, migrationLintSlots)
	}
}

// The route is registered behind the viewer-write gate: no session is
// 401, a viewer may lint.
func TestMigrationLintRouteGate(t *testing.T) {
	mux := http.NewServeMux()
	registerMigrationLintRoutes(mux, nil, nil)
	rec := postLint(t, mux, nil, `{"sql":"SELECT 1","static":true}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	rec = postLint(t, mux, testViewerUser(), `{"sql":"SELECT 1","static":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer: %d %s", rec.Code, rec.Body)
	}
	rec = postLint(t, mux, testViewerUser(), `{"sql":"SELECT 1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no fleet, live lint: %d %s", rec.Code, rec.Body)
	}
}

// stallCatalog blocks every read until its context ends, as a database
// that stopped answering would.
type stallCatalog struct{}

func (stallCatalog) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("stallCatalog: Query not supported")
}

func (stallCatalog) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	<-ctx.Done()
	return stallRow{err: ctx.Err()}
}

type stallRow struct{ err error }

func (r stallRow) Scan(...any) error { return r.err }

// Boundary: a review that outlives the request budget is 503 naming the
// budget, never a verdict, and gives its slot back.
func TestMigrationLintBudgetIs503(t *testing.T) {
	d := lintDeps(&lintCatalog{}, 30)
	d.querier = func(string) (catalogread.Querier, error) { return stallCatalog{}, nil }
	d.budget = 50 * time.Millisecond
	h := migrationLintHandler(d)
	start := time.Now()
	rec := postLint(t, h, testViewerUser(), `{"sql":"CREATE INDEX x ON app.t (c)"}`)
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "budget") ||
		strings.Contains(rec.Body.String(), "verdict") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the budget did not bound the review: %v", took)
	}
	if n := len(d.slots); n != 0 {
		t.Fatalf("%d slots still held", n)
	}
	if got := (migrationLintDeps{}).reviewBudget(); got != migrationLintBudget {
		t.Fatalf("zero budget is %v, want the default %v", got, migrationLintBudget)
	}
}
