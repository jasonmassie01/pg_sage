package config

import (
	"context"
	"fmt"
	"time"
)

// DurableSource reads the global desired revision that a control database
// shares between replicas: meta mode, and standalone replicas on one
// database (config-05).
type DurableSource interface {
	// DurableGeneration returns the durable global CAS generation.
	DurableGeneration(context.Context) (uint64, error)
	// DurableCandidate returns the current file base with the durable
	// global overrides replayed onto it, as startup builds it.
	DurableCandidate(context.Context) (*Config, error)
}

type durableBinding struct{ source DurableSource }

// SetDurableSource binds the shared revision that ResyncFromDurable reads;
// nil unbinds it. A YAML fleet binds none: it shares no overrides.
func (c *ConfigController) SetDurableSource(source DurableSource) {
	if source == nil {
		c.durable.Store(nil)
		return
	}
	c.durable.Store(&durableBinding{source: source})
}

// LastResync returns when this controller last adopted a generation that
// another writer saved; the zero time when it never has.
func (c *ConfigController) LastResync() time.Time {
	if at := c.lastResync.Load(); at != nil {
		return *at
	}
	return time.Time{}
}

// ResyncFromDurable reads the durable generation and, only when it is ahead
// of Desired, the candidate, which Resync then adopts. It reports whether
// the controller moved. Without a durable source it does nothing; on any
// error both snapshots stay as they were.
func (c *ConfigController) ResyncFromDurable(
	ctx context.Context,
) (ApplyResult, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	binding := c.durable.Load()
	if binding == nil {
		return c.currentResult(), false, nil
	}
	generation, err := binding.source.DurableGeneration(ctx)
	if err != nil {
		return ApplyResult{}, false, fmt.Errorf(
			"read durable config generation: %w", err)
	}
	if generation <= c.Desired().Generation {
		return c.currentResult(), false, nil
	}
	candidate, err := binding.source.DurableCandidate(ctx)
	if err != nil {
		return ApplyResult{}, false, fmt.Errorf(
			"load durable config generation %d: %w", generation, err)
	}
	return c.Resync(ctx, generation, candidate)
}

// Resync adopts durableGeneration, which another writer already persisted,
// through the same validate, prepare and commit path as Apply, under the
// same applyMu, but persisting nothing. Desired and active move to the
// durable generation (active only for the fields an owner applies). A
// generation at or behind Desired changes nothing and calls no owner. It
// reports whether desired adopted the generation.
func (c *ConfigController) Resync(
	ctx context.Context, durableGeneration uint64, candidate *Config,
) (ApplyResult, bool, error) {
	ctx, candidate, err := checkedCandidate(ctx, candidate)
	if err != nil {
		return ApplyResult{}, false, err
	}
	c.applyMu.Lock()
	active := c.Active()
	desired := c.Desired()
	if durableGeneration <= desired.Generation {
		c.applyMu.Unlock()
		return unchangedResult(desired.Generation, active.Generation), false, nil
	}
	adopted := false
	// Called under applyMu exactly when desired takes the generation.
	markAdopted := func(context.Context, ConfigSnapshot) error {
		now := time.Now()
		c.lastResync.Store(&now)
		adopted = true
		return nil
	}
	result, err := c.applyLocked(
		ctx, active, candidate, durableGeneration, markAdopted)
	if err != nil {
		return ApplyResult{}, false, err
	}
	return result, adopted, nil
}

func (c *ConfigController) currentResult() ApplyResult {
	return unchangedResult(c.Desired().Generation, c.Active().Generation)
}

// checkedCandidate detaches and validates a candidate before any writer
// takes applyMu; the error names the failing key.
func checkedCandidate(
	ctx context.Context, candidate *Config,
) (context.Context, *Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if candidate == nil {
		return ctx, nil, fmt.Errorf("candidate config is nil")
	}
	candidate = Clone(candidate)
	if err := candidate.validate(); err != nil {
		return ctx, nil, fmt.Errorf("%w: %w", ErrInvalidCandidate, err)
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	return ctx, candidate, nil
}
