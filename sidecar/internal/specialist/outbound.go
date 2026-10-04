package specialist

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The outbound queue: an adapter-opened investigation owes its system the
// result. The worker waits for the investigation to finish, then posts it
// through the notifier configured for that system. Only operator-configured
// endpoints are ever called; a system without one is marked failed.

// Notifier posts a result to one external system.
type Notifier interface {
	Deliver(ctx context.Context, rec Record, res Result) error
}

// OutboundWorker delivers owed results.
type OutboundWorker struct {
	Store     RequestStore
	Service   *Service
	Notifiers map[string]Notifier // by transport: pagerduty, webhook
	Now       func() time.Time
	// Interval is the wait before re-checking a running investigation and
	// the first retry backoff (doubled per attempt).
	Interval    time.Duration
	MaxAttempts int
	GiveUpAfter time.Duration
	Logger      *slog.Logger
}

const (
	claimLease = 2 * time.Minute
	claimBatch = 20
	maxBackoff = 30 * time.Minute
)

// Run ticks every Interval until ctx ends; errors are logged.
func (w *OutboundWorker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.logger().Warn("specialist: delivering results failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *OutboundWorker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

// Tick claims due posts and handles each; it returns how many it
// delivered.
func (w *OutboundWorker) Tick(ctx context.Context) (int, error) {
	now := w.Now()
	recs, err := w.Store.ClaimOutbound(ctx, now, claimLease, claimBatch)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, rec := range recs {
		ok, err := w.handle(ctx, rec, now)
		if err != nil {
			return delivered, err
		}
		if ok {
			delivered++
		}
	}
	return delivered, nil
}

// handle processes one claimed post; only store failures are returned.
func (w *OutboundWorker) handle(ctx context.Context, rec Record, now time.Time) (bool,
	error) {
	n, ok := w.Notifiers[rec.Transport]
	if !ok || n == nil {
		return false, w.Store.FinishOutbound(ctx, rec.ID, OutboundFailed,
			"outbound integration not configured for "+rec.Transport, now)
	}
	res, err := w.load(ctx, rec)
	if err != nil {
		return false, w.retry(ctx, rec, now, fmt.Sprintf("reading the result: %v", err))
	}
	if !res.Investigation.Terminal {
		if now.Sub(rec.CreatedAt) > w.GiveUpAfter {
			return false, w.Store.FinishOutbound(ctx, rec.ID, OutboundFailed,
				fmt.Sprintf("the investigation did not finish within %s", w.GiveUpAfter), now)
		}
		return false, w.Store.RescheduleOutbound(ctx, rec.ID, now.Add(w.Interval))
	}
	if err := n.Deliver(ctx, rec, res); err != nil {
		return false, w.retry(ctx, rec, now, sre.Scrub(err.Error()))
	}
	return true, w.Store.FinishOutbound(ctx, rec.ID, OutboundDelivered, "", now)
}

// retry counts a failed attempt: pending with exponential backoff, failed
// after MaxAttempts.
func (w *OutboundWorker) retry(ctx context.Context, rec Record, now time.Time,
	msg string) error {
	attempt := rec.OutboundAttempts + 1
	if attempt >= w.MaxAttempts {
		w.logger().Warn("specialist: giving up on a result post", "record", rec.ID,
			"transport", rec.Transport, "attempts", attempt, "err", msg)
		return w.Store.FinishOutbound(ctx, rec.ID, OutboundFailed, msg, now)
	}
	backoff := w.Interval << min(attempt, 10)
	return w.Store.FinishOutbound(ctx, rec.ID, OutboundPending, msg,
		now.Add(min(backoff, maxBackoff)))
}

// load reads the result as its requester would (its own record, the
// service's redaction), without spending the requester's rate limit.
func (w *OutboundWorker) load(ctx context.Context, rec Record) (Result, error) {
	b, ok := w.Service.dir.Backend(rec.Database)
	if !ok {
		return Result{}, fmt.Errorf("%w: database %q", ErrNotFound, rec.Database)
	}
	uid, err := parseInvestigationID(rec.InvestigationID)
	if err != nil {
		return Result{}, err
	}
	return w.Service.result(ctx, b, rec.TokenID, rec.Database, uid)
}
