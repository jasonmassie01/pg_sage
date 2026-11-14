package schema

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DeferredIndexBuilder runs BuildDeferredIndexes for one database until
// no declared deferred index is missing (config-04c), retrying with
// backoff from deferredBuilderFirstRetry to deferredBuilderMaxRetry. An
// emergency stop (Stopped) cancels a running build and holds new ones:
// the build is pg_sage DDL on its own schema, and the operator must see
// none during a stop.
type DeferredIndexBuilder struct {
	Pool     *pgxpool.Pool
	Database string
	// Stopped reports the database's emergency stop; nil never stops.
	Stopped func() bool
	// Observe receives the counts before and after each build; nil
	// ignores them.
	Observe func(DeferredIndexCounts)

	// Test seams; nil uses the real ones.
	build    func(context.Context) (BuildReport, error)
	states   func(context.Context) (DeferredIndexCounts, error)
	sleep    func(context.Context, time.Duration) bool
	warn     func(string, ...any)
	info     func(string, ...any)
	stopPoll time.Duration
}

const (
	deferredBuilderFirstRetry = time.Minute
	deferredBuilderMaxRetry   = time.Hour
	deferredBuilderStopPoll   = time.Second
)

// deferredBuilderBackoff is the wait before retry n (0-based): 1 min,
// doubling, capped at 1 h.
func deferredBuilderBackoff(n int) time.Duration {
	if n <= 0 {
		return deferredBuilderFirstRetry
	}
	if n >= 6 { // 1 min << 6 is past the cap
		return deferredBuilderMaxRetry
	}
	return min(deferredBuilderFirstRetry<<n, deferredBuilderMaxRetry)
}

// Run builds until nothing is missing or ctx ends. It logs one WARN per
// failure streak; another sidecar building or a stop is not a failure.
func (b *DeferredIndexBuilder) Run(ctx context.Context) {
	b.defaults()
	failing := false
	retry := 0
	for {
		if b.stopped() {
			if !b.sleep(ctx, deferredBuilderFirstRetry) {
				return
			}
			continue
		}
		done, err := b.attempt(ctx)
		if done || ctx.Err() != nil {
			return
		}
		failing = b.noteFailure(err, failing)
		if !b.sleep(ctx, deferredBuilderBackoff(retry)) {
			return
		}
		retry++
	}
}

func (b *DeferredIndexBuilder) defaults() {
	if b.build == nil {
		b.build = func(ctx context.Context) (BuildReport, error) {
			return BuildDeferredIndexes(ctx, b.Pool)
		}
	}
	if b.states == nil {
		b.states = func(ctx context.Context) (DeferredIndexCounts, error) {
			return ReadDeferredIndexStates(ctx, b.Pool)
		}
	}
	if b.sleep == nil {
		b.sleep = sleepCtx
	}
	if b.warn == nil {
		b.warn = slog.With("database", b.Database).Warn
	}
	if b.info == nil {
		b.info = slog.With("database", b.Database).Info
	}
	if b.stopPoll <= 0 {
		b.stopPoll = deferredBuilderStopPoll
	}
}

func (b *DeferredIndexBuilder) stopped() bool { return b.Stopped != nil && b.Stopped() }

func (b *DeferredIndexBuilder) observe(c DeferredIndexCounts) {
	if b.Observe != nil {
		b.Observe(c)
	}
}

// attempt reads the counts, builds when any index needs it, and reads them
// again. done is true when nothing is missing.
func (b *DeferredIndexBuilder) attempt(ctx context.Context) (bool, error) {
	if c, err := b.states(ctx); err == nil {
		b.observe(c)
		if c.pending() == 0 {
			return true, nil
		}
	}
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go b.watchStop(bctx, cancel)
	_, err := b.build(bctx)
	cancel()
	c, serr := b.states(ctx)
	if serr == nil {
		b.observe(c)
		if c.pending() == 0 {
			return true, nil
		}
	}
	if err == nil {
		err = serr
	}
	return false, err
}

// watchStop cancels the build when the emergency stop engages.
func (b *DeferredIndexBuilder) watchStop(ctx context.Context, cancel context.CancelFunc) {
	if b.Stopped == nil {
		return
	}
	tick := time.NewTicker(b.stopPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if b.Stopped() {
				b.info("schema: emergency stop: deferred index build cancelled")
				cancel()
				return
			}
		}
	}
}

// noteFailure logs an unfinished attempt and returns whether a failure
// streak is running.
func (b *DeferredIndexBuilder) noteFailure(err error, failing bool) bool {
	switch {
	case err == nil:
		return false // built, but an index is still building elsewhere
	case errors.Is(err, ErrDeferredBuildBusy):
		b.info("schema: " + ErrDeferredBuildBusy.Error() + "; retrying later")
		return failing
	case b.stopped() && errors.Is(err, context.Canceled):
		return failing
	case !failing:
		b.warn("schema: deferred index build failed; retrying with backoff "+
			"(1 min to 1 h); the boot does not wait for it", "error", err)
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
