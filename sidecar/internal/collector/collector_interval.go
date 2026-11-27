package collector

import (
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// intervalResetter is the part of *time.Ticker the interval sync uses.
type intervalResetter interface {
	Reset(d time.Duration)
}

// Interval is the period the collector's ticker runs at: the configured
// collector.interval_seconds, or the dormant interval while the circuit
// breaker is dormant. It is 0 (unknown) before Run starts the ticker.
func (c *Collector) Interval() time.Duration {
	return time.Duration(c.period.Load())
}

// configuredInterval reads collector.interval_seconds under the
// hot-reload lock: self-config promotions and API or file edits write the
// runtime config while the collector runs.
func (c *Collector) configuredInterval() time.Duration {
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	return c.cfg.Collector.Interval()
}

// syncInterval moves the ticker to the configured interval when it differs
// from the period in use. It runs between cycles, so a promoted interval
// reaches the collector within one cycle, and leaving the dormant state
// applies the interval configured now. A non-positive value (which would
// panic the ticker) keeps the period in use.
func (c *Collector) syncInterval(t intervalResetter) {
	want := c.configuredInterval()
	current := c.Interval()
	if want <= 0 {
		c.logFn("WARN", "collector: collector.interval_seconds=%d is not positive; "+
			"keeping %ds", int64(want/time.Second), int64(current/time.Second))
		return
	}
	if want == current {
		return
	}
	c.usePeriod(t, want)
	c.logFn("INFO", "collector: interval changed %ds -> %ds",
		int64(current/time.Second), int64(want/time.Second))
}

// usePeriod resets the ticker to d and records it as the period in use.
func (c *Collector) usePeriod(t intervalResetter, d time.Duration) {
	t.Reset(d)
	c.period.Store(int64(d))
}
