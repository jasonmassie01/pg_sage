package sre

import (
	"context"
	"errors"
	"time"
)

// Run is the coordinator loop. Every trigger interval it verifies the
// store after an outage, binds the scope, starts investigations from
// triggers (automatic start only), queues pending work (queued, waiting
// or orphaned by a dead worker) and applies retention when due. It runs
// queued investigations one at a time, so a database has at most one
// investigation probing it. It returns when ctx ends.
func (c *Coordinator) Run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.TriggerInterval)
	defer ticker.Stop()
	var lastRetention time.Time
	for {
		c.tick(ctx, &lastRetention)
		c.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case id := <-c.queue:
			c.investigateLogged(ctx, id)
		}
	}
}

func (c *Coordinator) tick(ctx context.Context, lastRetention *time.Time) {
	if ctx.Err() != nil {
		return
	}
	if c.durability.Status().Degraded {
		if err := c.durability.Verify(ctx, c.store); err != nil {
			c.logFn("WARN", "sre: coordination store still unavailable: %v", err)
			return
		}
		c.logFn("INFO", "sre: coordination store reachable again")
	}
	scope, err := c.Bind(ctx)
	if err != nil {
		c.logFn("WARN", "sre: binding the database identity failed: %v", err)
		return
	}
	if c.cfg.AutomaticStart {
		c.pollTriggers(ctx)
	}
	c.queuePending(ctx, scope)
	if time.Since(*lastRetention) >= c.cfg.RetentionInterval {
		c.retain(ctx, scope)
		*lastRetention = time.Now()
	}
}

func (c *Coordinator) pollTriggers(ctx context.Context) {
	triggers, err := c.triggers.Triggers(ctx)
	if err != nil {
		c.logFn("WARN", "sre: reading investigation triggers failed: %v", err)
		return
	}
	for _, t := range triggers {
		if _, _, err := c.Start(ctx, t); err != nil {
			c.logFn("WARN", "sre: starting the %s investigation of %s failed: %v",
				t.Kind, t.CaseID, err)
		}
	}
}

func (c *Coordinator) queuePending(ctx context.Context, scope Scope) {
	ids, err := c.store.Pending(ctx, scope, pendingBatch)
	c.durability.Observe(err)
	if err != nil {
		c.logFn("WARN", "sre: listing pending investigations failed: %v", err)
		return
	}
	for _, id := range ids {
		c.enqueue(id)
	}
}

func (c *Coordinator) retain(ctx context.Context, scope Scope) {
	res, err := c.store.Purge(ctx, scope, c.cfg.Retention)
	c.durability.Observe(err)
	switch {
	case err != nil:
		c.logFn("WARN", "sre: retention failed: %v", err)
	case res.EvidencePurged+res.InvestigationsPurged > 0:
		c.logFn("INFO", "sre: retention purged evidence of %d and all of %d "+
			"investigations (tombstoned)", res.EvidencePurged, res.InvestigationsPurged)
	}
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
