package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fakeLakebase replaces databricksauth.ConfigurePool for startup tests. It
// answers each call with the next queued error (nil once the queue is empty)
// and checks that every attempt starts from a fresh pool config.
type fakeLakebase struct {
	mu        sync.Mutex
	errs      []error
	calls     int
	stale     int
	deadlines []time.Duration
}

func (f *fakeLakebase) configure(ctx context.Context, cfg *pgxpool.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if d, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(d))
	} else {
		f.deadlines = append(f.deadlines, -1)
	}
	if cfg.BeforeConnect != nil {
		f.stale++
	}
	// Mutate as the real hook installation does, so a reused config shows.
	cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return nil }
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func installFakeLakebase(t *testing.T, errs ...error) *fakeLakebase {
	t.Helper()
	f := &fakeLakebase{errs: errs}
	oldConfigure, oldBackoff := configureLakebasePool, startupBackoff
	configureLakebasePool, startupBackoff = f.configure, time.Millisecond
	t.Cleanup(func() { configureLakebasePool, startupBackoff = oldConfigure, oldBackoff })
	return f
}

// permanentResolveError has the shape databricksauth returns for a 403: the
// SDK error wrapped with ErrEndpointPermanent and the endpoint named.
func permanentResolveError() error {
	denied := &apierr.APIError{StatusCode: 403, Message: "no access to endpoint"}
	return fmt.Errorf("resolve Lakebase endpoint projects/p/branches/b/endpoints/e: %w: %w",
		databricksauth.ErrEndpointPermanent, denied)
}

func startupOpeners(dsn string) map[string]func() (*pgxpool.Pool, error) {
	return map[string]func() (*pgxpool.Pool, error){
		"meta":      func() (*pgxpool.Pool, error) { return connectMetaDB(dsn) },
		"monitored": func() (*pgxpool.Pool, error) { return connectMonitoredDB(dsn, 3) },
		"fleet": func() (*pgxpool.Pool, error) {
			return openFleetPoolAtStartup(fleetConfigFor(dsn))
		},
	}
}

func fleetConfigFor(dsn string) config.DatabaseConfig {
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		return config.DatabaseConfig{Name: "unparseable", Host: "invalid host"}
	}
	return config.DatabaseConfig{Name: "lakebase-retry", Host: parsed.Host,
		Port: int(parsed.Port), User: parsed.User, Password: parsed.Password,
		Database: parsed.Database, SSLMode: "disable", MaxConnections: 2}
}

func TestStartupStopsOnPermanentLakebaseError(t *testing.T) {
	const dsn = "postgres://test@127.0.0.1:1/test?sslmode=disable"
	for name, open := range startupOpeners(dsn) {
		t.Run(name, func(t *testing.T) {
			f := installFakeLakebase(t, permanentResolveError())
			pool, err := open()
			if pool != nil {
				pool.Close()
				t.Fatal("startup opened a pool after a permanent endpoint error")
			}
			if !errors.Is(err, databricksauth.ErrEndpointPermanent) {
				t.Fatalf("error = %v; want the permanent endpoint error", err)
			}
			if !strings.Contains(err.Error(), "projects/p/branches/b/endpoints/e") {
				t.Fatalf("error %q does not name the endpoint", err)
			}
			if f.calls != 1 {
				t.Fatalf("configure calls = %d; want exactly one", f.calls)
			}
		})
	}
}

func TestStartupRetriesTransientLakebaseErrorWithFreshConfig(t *testing.T) {
	const dsn = "postgres://test@127.0.0.1:1/test?sslmode=disable"
	unavailable := &apierr.APIError{StatusCode: 503, Message: "unavailable"}
	for name, open := range startupOpeners(dsn) {
		t.Run(name, func(t *testing.T) {
			errs := []error{unavailable, unavailable, unavailable, unavailable, unavailable}
			f := installFakeLakebase(t, errs...)
			pool, err := open()
			if pool != nil {
				pool.Close()
				t.Fatal("startup opened a pool although every resolve failed")
			}
			if !errors.Is(err, apierr.ErrTemporarilyUnavailable) {
				t.Fatalf("error = %v; want the last 503 wrapped", err)
			}
			if !strings.Contains(err.Error(), "after 5 attempts") {
				t.Fatalf("error %q does not report the attempts", err)
			}
			if f.calls != 5 || f.stale != 0 {
				t.Fatalf("configure calls = %d, stale configs = %d; want 5 fresh", f.calls, f.stale)
			}
			for i, d := range f.deadlines {
				if d <= 0 || d > startupResolveTimeout {
					t.Fatalf("attempt %d resolve deadline = %s; want at most %s",
						i+1, d, startupResolveTimeout)
				}
			}
		})
	}
}

func TestStartupRecoversAfterTwoTransientResolveFailures(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	unavailable := &apierr.APIError{StatusCode: 503, Message: "unavailable"}
	for name, open := range startupOpeners(dsn) {
		t.Run(name, func(t *testing.T) {
			f := installFakeLakebase(t, unavailable, unavailable)
			pool, err := open()
			if err != nil {
				t.Fatalf("startup after two 503s = %v; want success", err)
			}
			defer pool.Close()
			var one int
			if err := pool.QueryRow(t.Context(), "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("recovered pool query = %d, %v", one, err)
			}
			if f.calls != 3 || f.stale != 0 {
				t.Fatalf("configure calls = %d, stale = %d; want 3 fresh", f.calls, f.stale)
			}
		})
	}
}

func TestRetryStartupHonorsCancellation(t *testing.T) {
	oldBackoff := startupBackoff
	startupBackoff = time.Hour
	t.Cleanup(func() { startupBackoff = oldBackoff })
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	open := func(context.Context) (*pgxpool.Pool, error) {
		calls++
		cancel()
		return nil, errors.New("connection refused")
	}
	start := time.Now()
	pool, err := retryStartup(ctx, "test", startupAttempts, open)
	if pool != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup = %v, %v; want context.Canceled", pool, err)
	}
	if calls != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("calls = %d after %s; want one attempt and no full backoff",
			calls, time.Since(start))
	}
}

func TestFleetReloadPoolMakesOneAttempt(t *testing.T) {
	const dsn = "postgres://test@127.0.0.1:1/test?sslmode=disable"
	unavailable := &apierr.APIError{StatusCode: 503, Message: "unavailable"}
	f := installFakeLakebase(t, unavailable, unavailable)
	pool, err := openFleetPool(fleetConfigFor(dsn))
	if pool != nil {
		pool.Close()
		t.Fatal("reload opened a pool although the resolve failed")
	}
	if !errors.Is(err, apierr.ErrTemporarilyUnavailable) ||
		strings.Contains(err.Error(), "attempts") {
		t.Fatalf("reload error = %v; want the 503 itself, unretried", err)
	}
	if f.calls != 1 {
		t.Fatalf("configure calls = %d; want one for an operator-visible reload", f.calls)
	}
}

// No concurrent access test: startup connects each pool from one goroutine;
// the credential hook's concurrency is tested in internal/databricksauth.
