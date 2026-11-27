package selfbudget

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LLMWaitLoop is the loop name under which the time tracked runs spent
// waiting on the LLM provider is recorded: it is not local work, so it
// does not rank in top_loops (LocalWork drops it).
const LLMWaitLoop = "llm_wait"

type ctxKey int

const (
	databaseKey ctxKey = iota
	runKey
)

// WithDatabase is ctx carrying the monitored database its loops work for.
// The database runtime sets it once; every worker it starts inherits it.
func WithDatabase(ctx context.Context, database string) context.Context {
	return context.WithValue(ctx, databaseKey, database)
}

// DatabaseOf is the database ctx carries ("" for a process loop).
func DatabaseOf(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	db, _ := ctx.Value(databaseKey).(string)
	return db
}

// LoopName is the tracker key of component on database: component alone
// for a process loop.
func LoopName(component, database string) string {
	if database == "" {
		return component
	}
	return component + ":" + database
}

// SplitLoopName is LoopName's inverse. Component names hold no colon, so
// the first one separates them; a database name may hold more.
func SplitLoopName(name string) (component, database string) {
	component, database, _ = strings.Cut(name, ":")
	return component, database
}

// run is one tracked run: the LLM wait of its calls and the wall time of
// the runs nested inside it, in nanoseconds.
type run struct {
	wait   atomic.Int64
	nested atomic.Int64
}

// AddWait adds d to the LLM wait of the run ctx carries. Without a
// tracked run in ctx, or for a non-positive d, it does nothing.
func AddWait(ctx context.Context, d time.Duration) {
	if r := runOf(ctx); r != nil && d > 0 {
		r.wait.Add(int64(d))
	}
}

func runOf(ctx context.Context) *run {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(runKey).(*run)
	return r
}

// TrackFor starts a run of component on the database ctx carries and
// returns the ctx the run's work must use (it carries the run's LLM wait)
// and the function that ends the run as a success. The run's busy time is
// its wall time minus its LLM wait and minus the runs nested inside it,
// never below zero; the wait is recorded under llm_wait:<database>.
// Ending a run twice counts it once.
func (l *Loops) TrackFor(ctx context.Context, component string) (context.Context, func()) {
	ctx, done := l.TrackForErr(ctx, component)
	return ctx, func() { done(nil) }
}

// TrackForErr is TrackFor for a loop whose run can fail: the result ends
// the run with its error, nil a success and anything else a failed run of
// the loop (observability-06).
func (l *Loops) TrackForErr(
	ctx context.Context, component string,
) (context.Context, func(error)) {
	if ctx == nil {
		ctx = context.Background()
	}
	if l == nil {
		return ctx, func(error) {}
	}
	database, parent := DatabaseOf(ctx), runOf(ctx)
	r := &run{}
	start := l.clock()
	var once sync.Once
	return context.WithValue(ctx, runKey, r), func(err error) {
		once.Do(func() {
			wall := l.clock().Sub(start)
			wait := time.Duration(r.wait.Load())
			busy := wall - wait - time.Duration(r.nested.Load())
			if busy < 0 {
				busy = 0
			}
			l.record(LoopName(component, database), busy, err)
			if wait > 0 {
				l.Add(LoopName(LLMWaitLoop, database), wait)
			}
			if parent != nil && wall > 0 {
				parent.nested.Add(int64(wall))
			}
		})
	}
}

// LocalWork is a copy of snap without the LLM-wait entries: the work
// pg_sage did itself, which top_loops ranks.
func LocalWork(snap map[string]LoopStat) map[string]LoopStat {
	out := make(map[string]LoopStat, len(snap))
	for name, s := range snap {
		if component, _ := SplitLoopName(name); component != LLMWaitLoop {
			out[name] = s
		}
	}
	return out
}

// ForDatabase is a copy of snap's loops of database.
func ForDatabase(snap map[string]LoopStat, database string) map[string]LoopStat {
	out := map[string]LoopStat{}
	for name, s := range snap {
		if _, db := SplitLoopName(name); db == database {
			out[name] = s
		}
	}
	return out
}

// ForgetDatabase drops the loops of a database that left the fleet (its
// component:database keys, llm_wait included), so their series leave
// /metrics with it. Process loops and other databases' loops are kept; an
// empty name forgets nothing.
func (l *Loops) ForgetDatabase(database string) {
	if l == nil || database == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for name := range l.stats {
		if _, db := SplitLoopName(name); db == database {
			delete(l.stats, name)
		}
	}
}
