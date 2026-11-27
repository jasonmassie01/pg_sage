package probes

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Caller is the class of work a probe runs for. The probe slots are
// shared by all of them; the queue metrics say which class waited for a
// slot or was refused one (sre-06).
type Caller string

// Probe caller classes.
const (
	CallerInvestigation Caller = "investigation"
	CallerDetector      Caller = "detector"
	CallerRunway        Caller = "runway"
	CallerLogRCA        Caller = "log_rca"
	CallerNarration     Caller = "narration"
	CallerActionTarget  Caller = "action_target"
	// CallerOther is a probe whose context names no class.
	CallerOther Caller = "other"
)

// The limiters a probe queues at: the database's one slot, then one of
// the sidecar's MaxSidecarConcurrency slots.
const (
	LimiterDatabase = "database"
	LimiterSidecar  = "sidecar"
)

type callerKey struct{}

// WithCaller tags ctx with the class of the probes run under it.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerOf is the class ctx was tagged with, CallerOther when none.
func CallerOf(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok && c != "" {
		return c
	}
	return CallerOther
}

// QueueRefused reports whether res found no free probe slot within
// QueueWait: the probe never ran, so another try may observe.
func QueueRefused(res Result) bool {
	return res.Phase == PhaseQueue && res.Reason == "concurrency_limit" &&
		!res.Status.Usable()
}

// QueueStat is one series of the probe queue metrics: the waits at one
// limiter (granted or refused), their total time and the refusals.
type QueueStat struct {
	Database    string
	Caller      Caller
	Limiter     string
	Waits       uint64
	WaitSeconds float64
	Refusals    uint64
}

type queueKey struct {
	database string
	caller   Caller
	limiter  string
}

var queueStats = struct {
	mu sync.Mutex
	m  map[queueKey]*QueueStat
}{m: map[queueKey]*QueueStat{}}

// observeQueue records one wait at a limiter and whether it was refused.
// A runner with no database name (an embedder's) is not exported: every
// series carries the database it belongs to, as the park counters do.
func observeQueue(database string, c Caller, limiter string, wait time.Duration,
	refused bool) {
	if database == "" {
		return
	}
	k := queueKey{database: database, caller: c, limiter: limiter}
	queueStats.mu.Lock()
	defer queueStats.mu.Unlock()
	s := queueStats.m[k]
	if s == nil {
		s = &QueueStat{Database: database, Caller: c, Limiter: limiter}
		queueStats.m[k] = s
	}
	s.Waits++
	s.WaitSeconds += wait.Seconds()
	if refused {
		s.Refusals++
	}
}

// ForgetQueueStats drops every series of a database removed from the
// fleet, so /metrics stops reporting it; a database added back starts at
// zero. An empty name changes nothing: it is not a removed database.
func ForgetQueueStats(database string) {
	if database == "" {
		return
	}
	queueStats.mu.Lock()
	defer queueStats.mu.Unlock()
	for k := range queueStats.m {
		if k.database == database {
			delete(queueStats.m, k)
		}
	}
}

// QueueStats is a sorted snapshot of the probe queue metrics.
func QueueStats() []QueueStat {
	queueStats.mu.Lock()
	out := make([]QueueStat, 0, len(queueStats.m))
	for _, s := range queueStats.m {
		out = append(out, *s)
	}
	queueStats.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Database != b.Database {
			return a.Database < b.Database
		}
		if a.Caller != b.Caller {
			return a.Caller < b.Caller
		}
		return a.Limiter < b.Limiter
	})
	return out
}

// timedAcquire takes a slot of l, recording the wait under the runner's
// database label and the context's caller class. Only a wait that ran
// out of time is a refusal; a caller that gave up (cancelled) was not
// refused a slot, though its wait counts.
func (r *Runner) timedAcquire(ctx context.Context, l *Limiter, limiter string) error {
	began := time.Now()
	err := l.acquire(ctx)
	observeQueue(r.databaseLabel(), CallerOf(ctx), limiter, time.Since(began),
		errors.Is(err, context.DeadlineExceeded))
	return err
}

// SetDatabaseName names the database in this runner's queue metrics (the
// identity the sidecar reports it under). Call it before the runner is
// shared; without it the pool's database name is used.
func (r *Runner) SetDatabaseName(name string) {
	if r != nil {
		r.name = name
	}
}

func (r *Runner) databaseLabel() string {
	if r.name != "" || r.pool == nil {
		return r.name
	}
	return r.pool.Config().ConnConfig.Database
}

// QueueWait bounds how long one probe waits for its slots.
func (r *Runner) QueueWait() time.Duration {
	if r == nil || r.queueWait <= 0 {
		return QueueWait
	}
	return r.queueWait
}
