package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// firstlook-05: an operator who followed the enablement steps (CREATE
// EXTENSION, a GRANT) re-runs the first look instead of restarting
// pg_sage. Operator-started only, at most once a minute per database, so
// the probe load stays predictable.

// firstLookRecheckEvery is how often one database may be re-checked.
const firstLookRecheckEvery = time.Minute

// recheckHook runs one database's first look again.
type recheckHook func(context.Context) (firstlook.Report, error)

// recheckSource finds a database's re-check; found is false for an
// unknown database, a nil hook means its runtime takes no first look.
type recheckSource interface {
	RecheckHook(name string) (hook recheckHook, found bool)
}

// fleetRecheckSource reads the hook from the database's fleet instance.
type fleetRecheckSource struct{ mgr *fleet.DatabaseManager }

func (f fleetRecheckSource) RecheckHook(name string) (recheckHook, bool) {
	if f.mgr == nil {
		return nil, false
	}
	inst := f.mgr.GetInstance(name)
	if inst == nil {
		return nil, false
	}
	if inst.FirstLookRecheck == nil {
		return nil, true
	}
	return inst.FirstLookRecheck, true
}

// recheckLimiter admits one re-check per database per window, in process.
// Only databases that exist reach it, so its map is bounded by the fleet.
type recheckLimiter struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	last   map[string]time.Time
}

func newRecheckLimiter(window time.Duration, now func() time.Time) *recheckLimiter {
	if now == nil {
		now = time.Now
	}
	return &recheckLimiter{window: window, now: now, last: map[string]time.Time{}}
}

// allow records a re-check of name when the window since the last one has
// passed; otherwise it returns how long is left.
func (l *recheckLimiter) allow(name string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if last, ok := l.last[name]; ok {
		if wait := l.window - now.Sub(last); wait > 0 {
			return false, wait
		}
	}
	l.last[name] = now
	return true, 0
}

// retryAfterSeconds is wait in whole seconds, rounded up, at least 1.
func retryAfterSeconds(wait time.Duration) int {
	secs := int((wait + time.Second - 1) / time.Second)
	return max(secs, 1)
}

// registerFirstLookRecheck serves the re-check to operators and admins.
func registerFirstLookRecheck(mux *http.ServeMux, src recheckSource,
	lim *recheckLimiter) {
	route(mux, "POST /api/v1/first-look/recheck", gateOperator,
		firstLookRecheckHandler(src, lim))
}

func firstLookRecheckHandler(src recheckSource, lim *recheckLimiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook, name, ok := resolveRecheck(w, r, src)
		if !ok {
			return
		}
		if allowed, wait := lim.allow(name); !allowed {
			secs := retryAfterSeconds(wait)
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{
				"error": fmt.Sprintf("the first look of %q was re-checked less than %d s "+
					"ago; try again in %d s", name, int(lim.window/time.Second), secs),
				"retry_after_seconds": secs})
			return
		}
		report, err := hook(r.Context())
		if err != nil {
			class := entryErrorCode(err)
			slog.Warn("first look re-check failed", "database", name, "class", class,
				"error", err)
			writeJSONStatus(w, http.StatusBadGateway, map[string]any{
				"error": "first look re-check failed; the previous report stands",
				"class": class})
			return
		}
		sum := firstlook.SummaryOf(report)
		jsonResponse(w, map[string]any{"database": name,
			"first_look": firstLookSummaryOf(&sum)})
	})
}

// resolveRecheck writes 400/404/503 and returns false when the request
// names no single database with a re-check.
func resolveRecheck(w http.ResponseWriter, r *http.Request, src recheckSource) (
	recheckHook, string, bool) {
	name, ok := readDatabaseParam(w, r)
	if !ok {
		return nil, "", false
	}
	if name == "" || name == "all" {
		jsonError(w, "choose one database to re-check (?database=)", http.StatusBadRequest)
		return nil, "", false
	}
	hook, found := src.RecheckHook(name)
	if !found {
		jsonError(w, "database not found", http.StatusNotFound)
		return nil, "", false
	}
	if hook == nil {
		jsonError(w, fmt.Sprintf("database %q has no first look to re-check: its "+
			"runtime is starting or stopped; try again once it is monitored", name),
			http.StatusServiceUnavailable)
		return nil, "", false
	}
	return hook, name, true
}
