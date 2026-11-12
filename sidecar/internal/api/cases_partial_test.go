package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// casesWire is the part of the cases response these tests read.
type casesWire struct {
	Database    string                     `json:"database"`
	Cases       []cases.Case               `json:"cases"`
	Total       int                        `json:"total"`
	Unavailable []casesUnavailable         `json:"unavailable"`
	Truncated   map[string]casesTruncation `json:"truncated"`
	Focus       *caseFocus                 `json:"focus"`
	ErrorClass  string                     `json:"error_class"`
	Error       string                     `json:"error"`
}

// lazyPool is a pool that never connects unless used; the seam-based
// tests never use it.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seamManager(t *testing.T, names ...string) *fleet.DatabaseManager {
	t.Helper()
	mgr := fleet.NewManager(caseAutonomousConfig())
	for _, name := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Config: config.DatabaseConfig{Name: name},
			Pool: lazyPool(t),
		})
	}
	return mgr
}

// stubDatabaseCases replaces the per-database read for one test.
func stubDatabaseCases(
	t *testing.T,
	fn func(ctx context.Context, sel namedPool) (databaseCases, error),
) {
	t.Helper()
	prev := readDatabaseCases
	readDatabaseCases = func(
		ctx context.Context, _ *fleet.DatabaseManager, sel namedPool,
	) (databaseCases, error) {
		return fn(ctx, sel)
	}
	t.Cleanup(func() { readDatabaseCases = prev })
}

func shortCasesTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := casesDatabaseTimeout
	casesDatabaseTimeout = d
	t.Cleanup(func() { casesDatabaseTimeout = prev })
}

var seamObservedAt = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func seamCase(database, id string) cases.Case {
	return cases.Case{
		ID: "finding:" + database + ":" + id, DatabaseName: database,
		Severity: cases.SeverityWarning, ObservedAt: seamObservedAt,
	}
}

func getCases(t *testing.T, mgr *fleet.DatabaseManager, query string) (int, casesWire) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cases"+query, nil)
	rec := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rec, req)
	var body casesWire
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func caseIDs(list []cases.Case) []string {
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = c.ID
	}
	return out
}

func TestCasesFleetAnswersPartialWhenOneDatabaseFails(t *testing.T) {
	mgr := seamManager(t, "alpha", "beta")
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		if sel.name == "beta" {
			return databaseCases{}, fmt.Errorf("query findings: %w",
				errors.New("failed to connect: dial tcp 10.0.0.9:5432: connection refused"))
		}
		return databaseCases{cases: []cases.Case{seamCase("alpha", "a1")}}, nil
	})

	code, body := getCases(t, mgr, "")

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := caseIDs(body.Cases); len(got) != 1 || got[0] != "finding:alpha:a1" {
		t.Fatalf("cases = %v, want alpha's one case", got)
	}
	if body.Total != 1 {
		t.Fatalf("total = %d, want 1", body.Total)
	}
	want := casesUnavailable{Database: "beta", ErrorClass: caseErrorConnection}
	if len(body.Unavailable) != 1 || body.Unavailable[0] != want {
		t.Fatalf("unavailable = %+v, want [%+v]", body.Unavailable, want)
	}
}

func TestCasesUnavailableCarriesNoRawErrorText(t *testing.T) {
	mgr := seamManager(t, "alpha")
	stubDatabaseCases(t, func(context.Context, namedPool) (databaseCases, error) {
		return databaseCases{}, errors.New(`password authentication failed for user "secret_role"`)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cases", nil)
	rec := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "secret_role") ||
		strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("response leaks the raw error: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error_class":"query"`) {
		t.Fatalf("response %s lacks error_class query", rec.Body.String())
	}
}

func TestCasesSingleDatabaseFailureAnswers503(t *testing.T) {
	mgr := seamManager(t, "alpha", "beta")
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		if sel.name == "beta" {
			return databaseCases{}, &pgconn.PgError{Code: "42P01", Message: "no table"}
		}
		return databaseCases{cases: []cases.Case{seamCase("alpha", "a1")}}, nil
	})

	code, body := getCases(t, mgr, "?database=beta")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if body.ErrorClass != caseErrorQuery || body.Database != "beta" {
		t.Fatalf("body = %+v, want database beta error_class query", body)
	}
	if strings.Contains(body.Error, "no table") {
		t.Fatalf("error %q leaks the raw message", body.Error)
	}

	code, body = getCases(t, mgr, "?database=alpha")
	if code != http.StatusOK || len(body.Cases) != 1 || len(body.Unavailable) != 0 {
		t.Fatalf("alpha = %d %+v, want 200 with its case and nothing unavailable",
			code, body)
	}
}

func TestCasesSlowDatabaseTimesOutWithinDeadline(t *testing.T) {
	const deadline = 300 * time.Millisecond
	shortCasesTimeout(t, deadline)
	mgr := seamManager(t, "alpha", "slow")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		if sel.name == "slow" {
			// Ignores its context, as a wedged driver call would.
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
			return databaseCases{cases: []cases.Case{seamCase("slow", "late")}}, nil
		}
		return databaseCases{cases: []cases.Case{seamCase("alpha", "a1")}}, nil
	})

	start := time.Now()
	code, body := getCases(t, mgr, "")
	elapsed := time.Since(start)

	if elapsed > deadline+time.Second {
		t.Fatalf("request took %v, want within %v", elapsed, deadline+time.Second)
	}
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	want := casesUnavailable{Database: "slow", ErrorClass: caseErrorTimeout}
	if len(body.Unavailable) != 1 || body.Unavailable[0] != want {
		t.Fatalf("unavailable = %+v, want [%+v]", body.Unavailable, want)
	}
	if got := caseIDs(body.Cases); len(got) != 1 || got[0] != "finding:alpha:a1" {
		t.Fatalf("cases = %v, want only alpha's", got)
	}
}

func TestCasesContextHonouringReadTimesOut(t *testing.T) {
	shortCasesTimeout(t, 100*time.Millisecond)
	mgr := seamManager(t, "alpha")
	stubDatabaseCases(t, func(ctx context.Context, _ namedPool) (databaseCases, error) {
		<-ctx.Done()
		return databaseCases{}, fmt.Errorf("query findings: %w", ctx.Err())
	})
	code, body := getCases(t, mgr, "?database=alpha")
	if code != http.StatusServiceUnavailable || body.ErrorClass != caseErrorTimeout {
		t.Fatalf("got %d %+v, want 503 timeout", code, body)
	}
}

// Nil/empty: no manager and a manager with no databases both answer an
// empty, well-formed list.
func TestCasesZeroDatabasesAnswerEmptyList(t *testing.T) {
	for name, mgr := range map[string]*fleet.DatabaseManager{
		"nil manager": nil, "no databases": seamManager(t),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/cases", nil)
			rec := httptest.NewRecorder()
			casesHandler(mgr).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			raw := rec.Body.String()
			for _, want := range []string{`"cases":[]`, `"unavailable":[]`,
				`"truncated":{}`, `"total":0`} {
				if !strings.Contains(raw, want) {
					t.Fatalf("body %s lacks %s", raw, want)
				}
			}
		})
	}
}

func TestCasesTruncatedNamesOnlyCutDatabases(t *testing.T) {
	mgr := seamManager(t, "alpha", "beta")
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		res := databaseCases{cases: []cases.Case{seamCase(sel.name, "x")}}
		if sel.name == "alpha" {
			res.truncated = casesTruncation{Findings: true, QueryHints: true}
		}
		return res, nil
	})
	_, body := getCases(t, mgr, "")
	want := casesTruncation{Findings: true, QueryHints: true}
	if len(body.Truncated) != 1 || body.Truncated["alpha"] != want {
		t.Fatalf("truncated = %+v, want only alpha %+v", body.Truncated, want)
	}
}

// Concurrent access: many databases are read in parallel, never more than
// casesMaxConcurrentDatabases at once, and the order is the same every run.
func TestCasesConcurrentReadsAreBoundedAndOrdered(t *testing.T) {
	names := make([]string, 12)
	for i := range names {
		names[i] = fmt.Sprintf("db%02d", i)
	}
	mgr := seamManager(t, names...)
	var inFlight, peak atomic.Int32
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(time.Duration(len(sel.name)%3+1) * 15 * time.Millisecond)
		return databaseCases{cases: []cases.Case{
			seamCase(sel.name, "b"), seamCase(sel.name, "a"),
		}}, nil
	})

	var first []string
	for run := 0; run < 5; run++ {
		got := caseIDs(collectCases(context.Background(), mgr, "all").Cases)
		if len(got) != 24 {
			t.Fatalf("run %d: %d cases, want 24", run, len(got))
		}
		if run == 0 {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d order %v differs from %v", run, got, first)
		}
	}
	if first[0] != "finding:db00:a" || first[1] != "finding:db00:b" {
		t.Fatalf("order starts %v, want db00's cases by id", first[:2])
	}
	// The bound is the literal 8 the plan names, not the constant, so a
	// change to the constant fails here.
	if p := peak.Load(); p > 8 || p < 2 {
		t.Fatalf("peak concurrent reads = %d, want 2..8", p)
	}
}

// State transition: a database that recovers drops out of unavailable on
// the next request.
func TestCasesRecoveredDatabaseLeavesUnavailable(t *testing.T) {
	mgr := seamManager(t, "alpha")
	var down sync.Mutex
	failing := true
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		down.Lock()
		defer down.Unlock()
		if failing {
			return databaseCases{}, errors.New("closed pool")
		}
		return databaseCases{cases: []cases.Case{seamCase(sel.name, "a")}}, nil
	})
	if _, body := getCases(t, mgr, ""); len(body.Unavailable) != 1 ||
		body.Unavailable[0].ErrorClass != caseErrorConnection {
		t.Fatalf("first unavailable = %+v, want alpha connection", body.Unavailable)
	}
	down.Lock()
	failing = false
	down.Unlock()
	if _, body := getCases(t, mgr, ""); len(body.Unavailable) != 0 || len(body.Cases) != 1 {
		t.Fatalf("after recovery = %+v, want one case and nothing unavailable", body)
	}
}

func TestClassifyCasesError(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), caseErrorTimeout},
		{"cancelled", context.Canceled, caseErrorTimeout},
		{"connect error", fmt.Errorf("x: %w", &pgconn.ConnectError{}), caseErrorConnection},
		{"net error", fmt.Errorf("x: %w", opErr), caseErrorConnection},
		{"sqlstate 08", &pgconn.PgError{Code: "08006"}, caseErrorConnection},
		{"closed pool text", errors.New("closed pool"), caseErrorConnection},
		{"undefined table", &pgconn.PgError{Code: "42P01"}, caseErrorQuery},
		{"short sqlstate", &pgconn.PgError{Code: "0"}, caseErrorQuery},
		{"plain", errors.New("scan finding: bad"), caseErrorQuery},
	}
	for _, tc := range cases {
		if got := classifyCasesError(tc.err); got != tc.want {
			t.Errorf("%s: class = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCasesFocusParameterValidation(t *testing.T) {
	long := "finding:" + strings.Repeat("x", maxCaseIdentityLen)
	for _, bad := range []string{"bogus:alpha:x", "finding:", "finding", long} {
		code, _ := getCases(t, nil, "?case="+bad)
		if code != http.StatusBadRequest {
			t.Errorf("case=%q status = %d, want 400", bad[:min(len(bad), 20)], code)
		}
	}
	edge := "finding:" + strings.Repeat("x", maxCaseIdentityLen-len("finding:"))
	if !validCaseIdentity(edge) {
		t.Fatalf("identity of exactly %d bytes rejected", maxCaseIdentityLen)
	}
	code, body := getCases(t, nil, "?case=incident:alpha:sig:1")
	if code != http.StatusOK || body.Focus == nil || body.Focus.Found {
		t.Fatalf("nil manager focus = %d %+v, want 200 not found", code, body.Focus)
	}
}

func TestCasesFocusOnPageIsNotReloaded(t *testing.T) {
	mgr := seamManager(t, "alpha")
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		return databaseCases{cases: []cases.Case{seamCase("alpha", "a")}}, nil
	})
	_, body := getCases(t, mgr, "?case=finding:alpha:a")
	if body.Focus == nil || !body.Focus.Found || body.Focus.Loaded {
		t.Fatalf("focus = %+v, want found on the page, not loaded", body.Focus)
	}
	if len(body.Cases) != 1 {
		t.Fatalf("cases = %d, want 1 (no duplicate)", len(body.Cases))
	}
}
