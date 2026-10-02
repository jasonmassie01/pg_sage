package sre

import (
	"context"
	"errors"
	"fmt"
)

// combinedTriggers polls several trigger sources in order.
type combinedTriggers struct {
	sources []TriggerSource
	logFn   func(level, msg string, args ...any)
}

// CombineTriggers merges trigger sources (RCA incidents, the reactive
// detector). A failing source is logged with its error and the others'
// triggers still start investigations; only when every source fails is
// the poll an error. Nil sources are skipped; logFn may be nil.
func CombineTriggers(logFn func(level, msg string, args ...any),
	sources ...TriggerSource) TriggerSource {
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	c := &combinedTriggers{logFn: logFn}
	for _, s := range sources {
		if s != nil {
			c.sources = append(c.sources, s)
		}
	}
	return c
}

// Triggers implements TriggerSource.
func (c *combinedTriggers) Triggers(ctx context.Context) ([]Trigger, error) {
	var out []Trigger
	var errs []error
	for i, s := range c.sources {
		ts, err := s.Triggers(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("trigger source %d of %d failed: %w", i+1,
				len(c.sources), err))
			continue
		}
		out = append(out, ts...)
	}
	if len(c.sources) > 0 && len(errs) == len(c.sources) {
		return nil, errors.Join(errs...) // the caller logs it
	}
	for _, err := range errs {
		c.logFn("WARN", "sre: reading %v; the other trigger sources still start "+
			"investigations", err)
	}
	return out, nil
}
