package sre

import (
	"fmt"
	"math"
	"time"
)

// leaseClock reads the database clock once per lease write, so the lease
// and segment remaining times returned with it are mutually consistent.
const leaseClock = `(SELECT clock_timestamp() AS ts) AS lease_clock`

// leaseLeftSQL returns the lease's and the segment's remaining seconds on
// the database clock. The database's absolute timestamps never reach a
// host-clock comparison (sre-12).
const leaseLeftSQL = `EXTRACT(EPOCH FROM lease_until - lease_clock.ts)::float8,
	EXTRACT(EPOCH FROM segment_deadline - lease_clock.ts)::float8`

// maxLeftSeconds is the largest remaining time a time.Duration can hold.
const maxLeftSeconds = float64(math.MaxInt64) / float64(time.Second)

// hostDeadline turns a remaining time read on the database clock into a
// host deadline anchored on sent, the moment the query left the host.
// The database read its clock after sent, so the deadline is never later
// than the database's, and it keeps sent's monotonic reading. A NaN,
// infinite or unrepresentable remaining is treated as no time left; a
// negative one gives a deadline in the past.
func hostDeadline(sent time.Time, leftSeconds float64) time.Time {
	if math.IsNaN(leftSeconds) || math.Abs(leftSeconds) >= maxLeftSeconds {
		return sent
	}
	return sent.Add(time.Duration(leftSeconds * float64(time.Second)))
}

// setDeadlines sets the lease's host deadlines from the database's
// remaining lease and segment times, both read when one query was sent.
func (l *Lease) setDeadlines(sent time.Time, untilLeft, segmentLeft float64) error {
	if sent.IsZero() {
		return fmt.Errorf("%w: lease deadlines need the time the lease query was sent",
			ErrInvalidRequest)
	}
	l.Until = hostDeadline(sent, untilLeft)
	l.SegmentDeadline = hostDeadline(sent, segmentLeft)
	return nil
}
