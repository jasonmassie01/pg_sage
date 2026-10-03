package autonomy

import "time"

// DefaultDDLDebounce is the least time between schema guard scans that
// pg_sage's own DDL requests (analyzer.schema_guard_ddl_debounce_seconds).
const DefaultDDLDebounce = time.Minute

func ddlDebounce(config DatabaseWorkersConfig) time.Duration {
	if config.DDLDebounce <= 0 {
		return DefaultDDLDebounce
	}
	return config.DDLDebounce
}

// ddlDebouncer coalesces DDL-requested schema guard scans: a request runs
// at once when the last scan finished at least window ago; otherwise one
// scan is scheduled for when the window ends and further requests join it.
// Any completed scan (periodic or DDL) satisfies a pending request. It is
// used only by its database worker goroutine.
type ddlDebouncer struct {
	window  time.Duration
	last    time.Time
	timer   *time.Timer
	pending <-chan time.Time
}

// request reports whether a scan should run now; otherwise one is (or
// already was) scheduled on due.
func (d *ddlDebouncer) request(now time.Time) bool {
	if d.pending != nil {
		return false
	}
	wait := d.window - now.Sub(d.last)
	if d.last.IsZero() || wait <= 0 {
		return true
	}
	d.timer = time.NewTimer(wait)
	d.pending = d.timer.C
	return false
}

// due fires when a scheduled scan should run; nil (blocks) when none is.
func (d *ddlDebouncer) due() <-chan time.Time { return d.pending }

// ran records a completed scan and drops any scheduled one.
func (d *ddlDebouncer) ran() {
	d.last = time.Now()
	d.cancel()
}

func (d *ddlDebouncer) cancel() {
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer, d.pending = nil, nil
}
