package schemaguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// A failed route (not a ParkedRoute) parks its item with the failure's
// class and the scan goes on, so one hot table cannot starve every
// invariant after it. The park's reason names only the class, so the
// ledger records a new row when the class changes, not every cycle. A
// connection failure stops the scan: every later route would fail too.
// After routeFailureLimit consecutive failures of one invariant its route
// is paused for routePauseScans scans.
const (
	routeFailureLimit = 3
	routePauseScans   = 6
)

// Route error classes, as named in a failed park's reason.
const (
	routeClassLockTimeout      = "lock_timeout"
	routeClassStatementTimeout = "statement_timeout"
	routeClassConnection       = "connection"
	routeClassPermission       = "permission"
	routeClassOther            = "other"
)

// routeFailure is a route that failed: item is its parked outcome.
type routeFailure struct {
	item  Remediation
	class string
	err   error
}

func (f *routeFailure) Error() string { return f.err.Error() }

func (f *routeFailure) Unwrap() error { return f.err }

// routeBackoff is one invariant's consecutive route failures; paused is
// how many more scans skip its route.
type routeBackoff struct {
	failures int
	paused   int
	class    string
}

// scanRun is what one scan's loop produced: the outcomes to record, the
// route errors, and the invariants a connection failure left unreached.
type scanRun struct {
	outcomes  []Remediation
	errs      []error
	unreached []Invariant
}

func (c *Custodian) processAll(
	ctx context.Context, invariants []Invariant, cycle scanCycle, result *CycleResult,
) (scanRun, error) {
	run := scanRun{outcomes: make([]Remediation, 0, len(invariants))}
	defer c.pruneBackoff(invariants)
	for i, invariant := range invariants {
		if err := ctx.Err(); err != nil {
			return run, err
		}
		if skipsIdle(invariant) {
			run.outcomes = append(run.outcomes, idleOutcome(invariant))
			result.Skipped++
			continue
		}
		item, err := c.process(ctx, invariant, cycle, result)
		var failure *routeFailure
		if errors.As(err, &failure) {
			run.outcomes = append(run.outcomes, failure.item)
			run.errs = append(run.errs, failure)
			if failure.class == routeClassConnection {
				run.stopAt(invariants[i+1:])
				return run, nil
			}
			continue
		}
		if err != nil {
			return run, err
		}
		run.outcomes = append(run.outcomes, item)
	}
	return run, nil
}

// stopAt marks the invariants not reached. An identity with an unreached
// member (a clone family) is not recorded this scan: a partial group's
// hash would differ from its full one and record a false change.
func (r *scanRun) stopAt(unreached []Invariant) {
	r.unreached = unreached
	skip := make(map[string]bool, len(unreached))
	for _, invariant := range unreached {
		skip[InvariantIdentity(invariant)] = true
	}
	kept := r.outcomes[:0]
	for _, item := range r.outcomes {
		if !skip[InvariantIdentity(item.Invariant)] {
			kept = append(kept, item)
		}
	}
	r.outcomes = kept
}

// route sends one remediation to the router unless its route is paused.
func (c *Custodian) route(
	ctx context.Context, item Remediation, result *CycleResult,
) (Remediation, error) {
	key := backoffKey(item.Invariant)
	if class, paused := c.pausedRoute(key); paused {
		item.Decision = parkedDecision(item.Decision, fmt.Sprintf(
			"routing paused after %d failures (%s)", routeFailureLimit, class))
		return item, nil
	}
	err := c.router.Route(ctx, item)
	if err == nil {
		c.routeSucceeded(key)
		result.Routed++
		return item, nil
	}
	var parked *ParkedRoute
	if errors.As(err, &parked) {
		item.Decision = parkedDecision(item.Decision, parked.Reason)
		return item, nil
	}
	class := routeErrorClass(err)
	c.routeFailed(key, class)
	item.Decision = parkedDecision(item.Decision, fmt.Sprintf(
		"schema remediation routing failed (%s); review runtime error before retrying",
		class))
	return item, &routeFailure{item: item, class: class, err: fmt.Errorf(
		"route schema remediation %s: %w", item.Invariant.Target(), err)}
}

// backoffKey is one member of an identity: clone family members share an
// identity but fail, and are paused, one table at a time.
func backoffKey(invariant Invariant) string {
	return InvariantIdentity(invariant) + "\x1f" + invariant.Target()
}

// pausedRoute reports whether key's route is paused, using up one paused
// scan, and the class of the failures that paused it.
func (c *Custodian) pausedRoute(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.backoff[key]
	if !ok || state.paused == 0 {
		return "", false
	}
	state.paused--
	c.backoff[key] = state
	return state.class, true
}

func (c *Custodian) routeSucceeded(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.backoff, key)
}

// routeFailed counts a failure. A failure at or past the limit (the first
// retry after a pause included) pauses the route again.
func (c *Custodian) routeFailed(key, class string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backoff == nil {
		c.backoff = map[string]routeBackoff{}
	}
	state := c.backoff[key]
	state.failures++
	state.class = class
	if state.failures >= routeFailureLimit {
		state.paused = routePauseScans
	}
	c.backoff[key] = state
}

// pruneBackoff forgets invariants that are no longer detected.
func (c *Custodian) pruneBackoff(invariants []Invariant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.backoff) == 0 {
		return
	}
	detected := make(map[string]bool, len(invariants))
	for _, invariant := range invariants {
		detected[backoffKey(invariant)] = true
	}
	for key := range c.backoff {
		if !detected[key] {
			delete(c.backoff, key)
		}
	}
}

// routeErrorClass names the kind of a route error for a failed park's
// reason. A connection error stops the scan.
func routeErrorClass(err error) string {
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return routeClassConnection
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "55P03":
			return routeClassLockTimeout
		case pgErr.Code == "57014":
			return routeClassStatementTimeout
		case strings.HasPrefix(pgErr.Code, "08"):
			return routeClassConnection
		case pgErr.Code == "42501":
			return routeClassPermission
		}
		return routeClassOther
	}
	// A context deadline satisfies net.Error but is not a lost connection.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return routeClassOther
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return routeClassConnection
	}
	return routeClassOther
}
