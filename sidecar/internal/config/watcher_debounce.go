package config

import "time"

// reloadDebounce coalesces a burst of directory events (an editor's
// truncate-then-write, the kubelet's ..data swap) into one reload.
const reloadDebounce = 300 * time.Millisecond

// reloadMaxWait bounds how long a steady stream of directory events, such as
// a log file written next to config.yaml, can postpone a reload check.
const reloadMaxWait = 2 * time.Second

// debouncer is a trailing-edge timer owned by the watcher's run goroutine.
// Each event restarts the quiet period, but never past reloadMaxWait after
// the first event of the burst.
type debouncer struct {
	timer   *time.Timer
	armed   bool
	firstAt time.Time
}

func newDebouncer() *debouncer {
	timer := time.NewTimer(reloadDebounce)
	timer.Stop()
	return &debouncer{timer: timer}
}

// arm restarts the timer for an event seen at now and returns the wait used.
func (d *debouncer) arm(now time.Time) time.Duration {
	if !d.armed {
		d.armed = true
		d.firstAt = now
	}
	wait := reloadDebounce
	if left := d.firstAt.Add(reloadMaxWait).Sub(now); left < wait {
		wait = max(left, 0)
	}
	d.timer.Reset(wait)
	return wait
}

// fired ends the burst, so the next event starts a new one.
func (d *debouncer) fired() { d.armed = false }

func (d *debouncer) stop() { d.timer.Stop() }
