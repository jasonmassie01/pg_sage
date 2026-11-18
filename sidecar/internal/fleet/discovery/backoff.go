package discovery

import (
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// A source whose provider throttles a listing (429 or 5xx) is not listed
// again until its back-off ends: 10 minutes, doubling to an hour while
// the provider keeps throttling. Any other failure is retried next cycle.
const (
	firstBackoff = 10 * time.Minute
	maxBackoff   = time.Hour
)

type backoffState struct {
	until time.Time
	step  time.Duration
}

func (r *Runner) backingOff(name string, now time.Time) bool {
	b, ok := r.backoff[name]
	return ok && now.Before(b.until)
}

// noteListing starts or extends name's back-off after a throttled listing
// and ends it after any other outcome.
func (r *Runner) noteListing(name string, err error, now time.Time) {
	if !clone.Throttled(err) {
		delete(r.backoff, name)
		return
	}
	step := firstBackoff
	if prev, ok := r.backoff[name]; ok {
		step = min(prev.step*2, maxBackoff)
	}
	r.backoff[name] = backoffState{until: now.Add(step), step: step}
}
