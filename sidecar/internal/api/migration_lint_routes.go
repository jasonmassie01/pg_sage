package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/migration"
)

// POST /api/v1/migrations/lint (migration-11) reviews migration SQL with
// the engine MCP lint_migration and the lint-migration CLI use. Any
// signed-in role may call it: the SQL is never run, and the catalog reads
// are bounded (statement_timeout 2 s, at most migrationLintSlots at once
// across all users) and rate limited per user. In fleet mode "database"
// selects the instance; an unknown name is 404.

const migrationLintPath = "/api/v1/migrations/lint"

const (
	// migrationLintSlots caps concurrent catalog reviews, so a burst of
	// lint requests cannot take the monitored database's pool.
	migrationLintSlots = 4
	// migrationLintMaxBody leaves room for JSON escaping of a 100 kB SQL.
	migrationLintMaxBody = 2*migration.MaxReviewBytes + 4096
	// migrationLintBudget bounds one whole review: each catalog read has
	// its own 2 s bound, but a long migration makes many, and a slot is
	// held for all of them.
	migrationLintBudget = 30 * time.Second
)

var migrationLintTimeouts = catalogread.Timeouts{Statement: 2 * time.Second, Lock: time.Second}

// Database resolution failures.
var (
	errLintUnknownDatabase  = errors.New("database not found")
	errLintNoDatabase       = errors.New("no connected database")
	errLintDatabaseRequired = errors.New("database is required: several databases are " +
		"monitored")
)

type migrationLintRequest struct {
	SQL       string `json:"sql"`
	PGVersion int    `json:"pg_version,omitempty"`
	Static    bool   `json:"static,omitempty"`
	Database  string `json:"database,omitempty"`
}

type migrationLintDeps struct {
	// querier resolves the database to its bounded catalog reader.
	querier      func(database string) (catalogread.Querier, error)
	rowThreshold int64
	limiter      *lintLimiter
	slots        chan struct{}
	// budget bounds one review; 0 is migrationLintBudget.
	budget time.Duration
}

func (d migrationLintDeps) reviewBudget() time.Duration {
	if d.budget <= 0 {
		return migrationLintBudget
	}
	return d.budget
}

func registerMigrationLintRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config) {
	deps := migrationLintDeps{querier: fleetLintQuerier(mgr),
		limiter: newLintLimiter(config.DefaultMigrationLintRatePerMinute, time.Minute),
		slots:   make(chan struct{}, migrationLintSlots)}
	if cfg != nil {
		deps.rowThreshold = int64(cfg.Migration.DDLRowThreshold)
		deps.limiter = newLintLimiter(cfg.API.MigrationLintRate(), time.Minute)
	}
	route(mux, "POST "+migrationLintPath, gateViewerWrite, migrationLintHandler(deps))
}

// fleetLintQuerier selects one monitored database: the named one, or the
// only one when a single database is monitored.
func fleetLintQuerier(mgr *fleet.DatabaseManager) func(string) (catalogread.Querier, error) {
	return func(database string) (catalogread.Querier, error) {
		if mgr == nil {
			return nil, errLintNoDatabase
		}
		if database == "" {
			names := mgr.Instances()
			if len(names) > 1 {
				return nil, errLintDatabaseRequired
			}
			for name := range names {
				database = name
			}
		}
		if database == "" {
			return nil, errLintNoDatabase
		}
		if mgr.GetInstance(database) == nil {
			return nil, errLintUnknownDatabase
		}
		pool := mgr.PoolForDatabase(database)
		if pool == nil {
			return nil, errLintNoDatabase
		}
		return catalogread.New(pool, migrationLintTimeouts), nil
	}
}

func migrationLintHandler(d migrationLintDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retry, ok := d.limiter.allow(lintLimiterKey(r), time.Now()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			jsonError(w, "migration lint rate limit reached: retry later",
				http.StatusTooManyRequests)
			return
		}
		req, err := decodeMigrationLint(w, r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		var q catalogread.Querier
		if !req.Static {
			if q, err = d.querier(req.Database); err != nil {
				jsonError(w, err.Error(), lintDatabaseStatus(err))
				return
			}
		}
		select {
		case d.slots <- struct{}{}:
			defer func() { <-d.slots }()
		case <-r.Context().Done():
			jsonError(w, "migration lint: request ended while waiting for a slot",
				http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), d.reviewBudget())
		defer cancel()
		rep, err := migration.Review(ctx, q, req.SQL, migration.ReviewOptions{
			PGVersion: req.PGVersion, RowThreshold: d.rowThreshold, Static: req.Static})
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("the review took longer than its %s budget: %w",
				d.reviewBudget(), err)
		}
		writeMigrationLint(w, rep, err)
	})
}

func lintDatabaseStatus(err error) int {
	switch {
	case errors.Is(err, errLintUnknownDatabase):
		return http.StatusNotFound
	case errors.Is(err, errLintDatabaseRequired):
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

func writeMigrationLint(w http.ResponseWriter, rep migration.Report, err error) {
	switch {
	case errors.Is(err, migration.ErrInvalidMigration):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		w.Header().Set("Retry-After", "5")
		jsonError(w, "migration not reviewed: "+err.Error(), http.StatusServiceUnavailable)
	default:
		writeJSONStatus(w, http.StatusOK, rep)
	}
}

// decodeMigrationLint reads the bounded body and checks the SQL.
func decodeMigrationLint(w http.ResponseWriter, r *http.Request) (migrationLintRequest, error) {
	var req migrationLintRequest
	body := http.MaxBytesReader(w, r.Body, migrationLintMaxBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return req, fmt.Errorf("the body is over %d bytes", migrationLintMaxBody)
		}
		return req, fmt.Errorf("the body must be JSON {\"sql\": ...}: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, errors.New("the body must be one JSON object")
	}
	if err := validateDatabaseParam(req.Database); err != nil {
		return req, err
	}
	return req, nil
}

// lintLimiterKey is the signed-in user (the gate guarantees one).
func lintLimiterKey(r *http.Request) string {
	if u := UserFromContext(r.Context()); u != nil {
		return "user:" + strconv.Itoa(u.ID)
	}
	return "anonymous"
}

// lintLimiter allows limit requests per key in any sliding window.
type lintLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	seen   map[string][]time.Time
}

func newLintLimiter(limit int, window time.Duration) *lintLimiter {
	return &lintLimiter{limit: limit, window: window, seen: map[string][]time.Time{}}
}

// allow records a request at now; when the key is over its limit it
// returns how long until the oldest request leaves the window.
func (l *lintLimiter) allow(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.seen[key] = kept
		return l.window - now.Sub(kept[0]), false
	}
	l.seen[key] = append(kept, now)
	return 0, true
}
