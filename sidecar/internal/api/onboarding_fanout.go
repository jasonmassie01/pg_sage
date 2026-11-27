package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Each database's onboarding read is bounded and they run side by side,
// so one unreachable or slow database cannot fail or stall the response
// for the rest of the fleet.
const (
	onboardingReadTimeout = 3 * time.Second
	onboardingWorkers     = 8
)

// Per-entry error codes. The cause stays in the server log.
const (
	entryNoRuntime   = "no_runtime"  // the database has no connection pool
	entryUnreachable = "unreachable" // the read could not reach it in time
	entryReadFailed  = "read_failed" // it answered, but the read failed
)

// errNoRuntime: the database is registered but has no runtime (its
// connection failed at startup, or it is not running).
var errNoRuntime = errors.New("database has no runtime")

// entryErrorCode classifies a per-database read error; "" for nil.
func entryErrorCode(err error) string {
	var netErr net.Error
	var connErr *pgconn.ConnectError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errNoRuntime):
		return entryNoRuntime
	case errors.Is(err, context.DeadlineExceeded), pgconn.Timeout(err),
		errors.As(err, &connErr), errors.As(err, &netErr):
		return entryUnreachable
	default:
		return entryReadFailed
	}
}

// readEach calls read for every name, on at most onboardingWorkers
// goroutines, each with its own onboardingReadTimeout context. read
// receives the name's index so it can fill its own slot.
func readEach(ctx context.Context, names []string,
	read func(ctx context.Context, i int, name string)) {
	sem := make(chan struct{}, onboardingWorkers)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rctx, cancel := context.WithTimeout(ctx, onboardingReadTimeout)
			defer cancel()
			read(rctx, i, name)
		}()
	}
	wg.Wait()
}

// entryError logs a database's read error with its name and returns the
// code the response carries.
func entryError(op, name string, err error) string {
	code := entryErrorCode(err)
	if code != "" {
		slog.Warn("onboarding: "+op, "database", name, "code", code, "error", err)
	}
	return code
}
