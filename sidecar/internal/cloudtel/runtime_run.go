package cloudtel

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"time"
)

const (
	// permanentBackoffCap bounds the wait after missing credentials, an
	// unknown identity or denied access. Polls continue, so credentials
	// added later are picked up within this long (a reload is sooner).
	permanentBackoffCap = 30 * time.Minute
	// transientBackoffCap bounds the wait after throttling, provider,
	// response, clock or network errors.
	transientBackoffCap = 10 * time.Minute
	// backoffJitter spreads transient retries (+/-20%) so databases that
	// failed together do not retry together.
	backoffJitter = 0.2
)

// Run polls until ctx ends, then invalidates the evidence. A success
// waits one interval; consecutive failures back off exponentially (see
// nextDelay). report is called when a failure begins or its class or
// reason changes, and onRecover when a poll succeeds after a reported
// failure; either may be nil. An Unavailable runtime has nothing to
// poll: Run waits for ctx and reports nothing. PollNow ends a wait early.
func (r *Runtime) Run(ctx context.Context, report func(error), onRecover func()) {
	defer r.invalidate()
	if r.src == nil {
		<-ctx.Done()
		return
	}
	reported := "" // the reported failure's class and reason; "" = healthy
	for {
		err := r.Poll(ctx)
		if ctx.Err() != nil {
			return
		}
		reported = notifyTransition(reported, err, report, onRecover)
		select {
		case <-ctx.Done():
			return
		case <-r.after(r.schedule(err)):
		case <-r.wake:
		}
	}
}

// notifyTransition reports a new or changed failure and a recovery, and
// returns the failure now reported ("" when healthy).
func notifyTransition(reported string, err error, report func(error),
	onRecover func()) string {
	if err == nil {
		if reported != "" && onRecover != nil {
			onRecover()
		}
		return ""
	}
	key := failureKey(err)
	if key != reported && report != nil {
		report(err)
	}
	return key
}

// requestIDPattern matches the per-request ID an SDK puts in its errors
// (AWS: "RequestID: 1b2c-..."); it changes on every call.
var requestIDPattern = regexp.MustCompile(`(?i)request ?id: *[^\s,;]+`)

// failureKey identifies a failure for de-duplication: its text, which
// names the class sentinel and the reason, minus per-request IDs, so an
// unchanged failure is not reported again on every retry.
func failureKey(err error) string {
	return "failed: " + requestIDPattern.ReplaceAllString(err.Error(), "RequestID: _")
}

// schedule records when Run polls next and returns the wait.
func (r *Runtime) schedule(err error) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.nextDelay(err, r.failures)
	r.nextPoll = r.now().Add(d)
	return d
}

// nextDelay is the wait before the next poll: the interval after a
// success; after n consecutive failures, interval x 2^(n-1), capped at
// 30 minutes for permanent errors and at 10 minutes, +/-20% jitter, for
// transient ones. A cap never shortens the configured interval.
func (r *Runtime) nextDelay(err error, failures int) time.Duration {
	if err == nil || failures < 1 {
		return r.interval
	}
	limit := transientBackoffCap
	if permanentError(err) {
		limit = permanentBackoffCap
	}
	limit = max(limit, r.interval)
	d := r.interval
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	if permanentError(err) {
		return d
	}
	spread := (rand.Float64()*2 - 1) * backoffJitter
	return d + time.Duration(float64(d)*spread)
}

// permanentError is a failure that retrying soon cannot fix: missing
// credentials, an unknown database identity or denied access.
func permanentError(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, ErrAuth)
}
