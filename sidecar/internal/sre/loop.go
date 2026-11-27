package sre

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// Run is the coordinator loop. Every trigger interval it verifies the
// store after an outage, binds the scope, starts investigations from
// triggers (automatic start only), queues pending work (queued, waiting
// or orphaned by a dead worker), sends settled diagnoses of incidents to
// the engineer who was paged (sre-08) and applies retention when due. It runs
// queued investigations one at a time, so a database has at most one
// investigation probing it. It returns when ctx ends.
func (c *Coordinator) Run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.TriggerInterval)
	defer ticker.Stop()
	var lastRetention time.Time
	for {
		c.trackedTick(ctx, &lastRetention)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case id := <-c.queue:
			c.investigateLogged(ctx, id)
		}
	}
}

// trackedTick is one run of the sre loop: the tick and the queued work it
// found, a failed run when the tick could not do its work
// (observability-06). A run cut short by ctx is not recorded.
func (c *Coordinator) trackedTick(ctx context.Context, lastRetention *time.Time) {
	ctx, done := c.loopTracker().TrackForErr(ctx, "sre")
	err := c.tick(ctx, lastRetention)
	c.drain(ctx)
	if ctx.Err() == nil {
		done(err)
	}
}

// loopTracker is the process-wide loop tracker unless a test replaced it.
func (c *Coordinator) loopTracker() *selfbudget.Loops {
	if c.loops != nil {
		return c.loops
	}
	return selfbudget.Process()
}

// tick returns why it could not do its work (each cause already logged);
// nil when it did, or when ctx had already ended.
func (c *Coordinator) tick(ctx context.Context, lastRetention *time.Time) error {
	if ctx.Err() != nil {
		return nil
	}
	if c.durability.Status().Degraded {
		if err := c.durability.Verify(ctx, c.store); err != nil {
			c.logFn("WARN", "sre: coordination store still unavailable: %v", err)
			return fmt.Errorf("verify the coordination store: %w", err)
		}
		c.logFn("INFO", "sre: coordination store reachable again")
	}
	scope, err := c.Bind(ctx)
	if err != nil {
		c.logFn("WARN", "sre: binding the database identity failed: %v", err)
		return fmt.Errorf("bind the database identity: %w", err)
	}
	var errs []error
	if c.cfg.AutomaticStart {
		errs = append(errs, c.pollTriggers(ctx))
	}
	errs = append(errs, c.queuePending(ctx, scope))
	c.notifyConclusions(ctx, scope)
	if time.Since(*lastRetention) >= c.cfg.RetentionInterval {
		errs = append(errs, c.retain(ctx, scope))
		*lastRetention = time.Now()
	}
	return errors.Join(errs...)
}

func (c *Coordinator) pollTriggers(ctx context.Context) error {
	triggers, err := c.triggers.Triggers(ctx)
	if err != nil {
		c.logFn("WARN", "sre: reading investigation triggers failed: %v", err)
		return fmt.Errorf("read investigation triggers: %w", err)
	}
	for _, t := range triggers {
		if _, _, err := c.Start(ctx, t); err != nil {
			c.logFn("WARN", "sre: starting the %s investigation of %s failed: %v",
				t.Kind, t.CaseID, err)
		}
	}
	return nil
}

func (c *Coordinator) queuePending(ctx context.Context, scope Scope) error {
	ids, err := c.store.Pending(ctx, scope, pendingBatch)
	c.durability.Observe(err)
	if err != nil {
		c.logFn("WARN", "sre: listing pending investigations failed: %v", err)
		return fmt.Errorf("list pending investigations: %w", err)
	}
	for _, id := range ids {
		c.enqueue(id)
	}
	return nil
}

func (c *Coordinator) retain(ctx context.Context, scope Scope) error {
	res, err := c.store.Purge(ctx, scope, c.cfg.Retention)
	c.durability.Observe(err)
	switch {
	case err != nil:
		c.logFn("WARN", "sre: retention failed: %v", err)
		return fmt.Errorf("apply retention: %w", err)
	case res.EvidencePurged+res.InvestigationsPurged > 0:
		c.logFn("INFO", "sre: retention purged evidence of %d and all of %d "+
			"investigations (tombstoned)", res.EvidencePurged, res.InvestigationsPurged)
	}
	return nil
}

// drain runs everything queued now.
func (c *Coordinator) drain(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case id := <-c.queue:
			c.investigateLogged(ctx, id)
		default:
			return
		}
	}
}

func (c *Coordinator) investigateLogged(ctx context.Context, id UUID) {
	err := c.Investigate(ctx, id)
	if err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		c.logFn("WARN", "sre: investigation %s: %v", id, err)
	}
}
