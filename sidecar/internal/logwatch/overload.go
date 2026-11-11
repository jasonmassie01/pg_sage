package logwatch

import (
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

// Drop reasons label pg_sage_logwatch_dropped_lines_total (rca-06).
const (
	DropReasonQueueOverflow = "queue_overflow"
	DropReasonClassifierCap = "classifier_cap"
	// DropReasonOversized is a record over its size limit (rca-04):
	// logwatch.max_line_len_bytes, or PlanRecordMaxBytes for a plan.
	DropReasonOversized = "oversized_record"
)

// DropCounts are the log lines one watcher lost since it started, by
// reason: evicted from the full line queue, skipped by
// logwatch.max_lines_per_cycle, or over their size limit.
type DropCounts struct {
	QueueOverflow uint64
	ClassifierCap uint64
	Oversized     uint64
}

// Total is every line lost, whatever the reason.
func (c DropCounts) Total() uint64 {
	return c.QueueOverflow + c.ClassifierCap + c.Oversized
}

type dropCounters struct {
	queue     atomic.Uint64
	capped    atomic.Uint64
	oversized atomic.Uint64
}

func (d *dropCounters) add(queue, capped int) {
	if queue > 0 {
		d.queue.Add(uint64(queue))
	}
	if capped > 0 {
		d.capped.Add(uint64(capped))
	}
}

func (d *dropCounters) addOversized(n int) {
	if n > 0 {
		d.oversized.Add(uint64(n))
	}
}

func (d *dropCounters) snapshot() DropCounts {
	return DropCounts{QueueOverflow: d.queue.Load(), ClassifierCap: d.capped.Load(),
		Oversized: d.oversized.Load()}
}

// overloadSignal is the cluster-wide warning a lossy drain emits, or nil
// when nothing was lost. It carries no database, so every engine sharing
// the watcher keeps it.
func overloadSignal(queue, capped int, now time.Time) *rca.Signal {
	if queue+capped <= 0 {
		return nil
	}
	return &rca.Signal{
		ID:       rca.SignalLogWatcherOverloaded,
		FiredAt:  now,
		Severity: "warning",
		Metrics: map[string]any{
			"dropped_lines":        queue + capped,
			"queue_overflow_lines": queue,
			"classifier_cap_lines": capped,
		},
	}
}
