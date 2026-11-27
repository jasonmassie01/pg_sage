package migration

import (
	"context"
	"sort"
	"sync"
)

// enrichQueueSize bounds each database's pending model work.
const enrichQueueSize = 16

// enrichJob is one detection's model work, done off the poll path: the
// script for a classified statement (risk set, written into the finding
// identity names) or the LLM fallback for an unclassified one (risk nil).
type enrichJob struct {
	pid      int
	sql      string
	risk     *DDLRisk
	identity string
	sink     FindingSink
}

// Enricher runs one database's model work on a single worker, so a slow
// model never stalls DDL detection (migration-10). The queue is bounded:
// when it is full the oldest job is dropped and counted.
type Enricher struct {
	db    string
	size  int
	run   func(context.Context, enrichJob)
	logFn func(string, string, ...any)
	wake  chan struct{}

	mu      sync.Mutex
	queue   []enrichJob
	dropped int64
}

func newEnricher(
	db string, size int, run func(context.Context, enrichJob),
	logFn func(string, string, ...any),
) *Enricher {
	return &Enricher{db: db, size: size, run: run, logFn: logFn,
		wake: make(chan struct{}, 1)}
}

// enqueue adds job, dropping the oldest pending job when the queue is full.
func (e *Enricher) enqueue(job enrichJob) {
	if e == nil {
		return
	}
	e.mu.Lock()
	dropped := len(e.queue) >= e.size
	if dropped {
		e.queue = append(e.queue[:0], e.queue[1:]...)
		e.dropped++
	}
	e.queue = append(e.queue, job)
	e.mu.Unlock()
	if dropped {
		enrichDrops.add(e.db)
		e.logFn("warn", "migration: enrichment queue for %s is full; "+
			"dropped the oldest model request", e.db)
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Enricher) next() (enrichJob, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 {
		return enrichJob{}, false
	}
	job := e.queue[0]
	e.queue = append(e.queue[:0], e.queue[1:]...)
	return job, true
}

// drain runs every pending job in queue order, stopping early when ctx ends.
func (e *Enricher) drain(ctx context.Context) {
	if e == nil {
		return
	}
	for ctx.Err() == nil {
		job, ok := e.next()
		if !ok {
			return
		}
		e.run(ctx, job)
	}
}

// Run works the queue until ctx is cancelled.
func (e *Enricher) Run(ctx context.Context) {
	if e == nil {
		return
	}
	for {
		e.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
		}
	}
}

// Dropped is how many jobs this enricher dropped because its queue was full.
func (e *Enricher) Dropped() int64 {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

// EnrichDropCount is one database's dropped model requests since start.
type EnrichDropCount struct {
	Database string
	Count    int64
}

// enrichDrops counts drops by database for /metrics.
var enrichDrops = dropCounter{byDB: map[string]int64{}}

type dropCounter struct {
	mu   sync.Mutex
	byDB map[string]int64
}

func (c *dropCounter) add(db string) {
	c.mu.Lock()
	c.byDB[db]++
	c.mu.Unlock()
}

// EnrichDroppedCounts lists every database's dropped model requests, by
// name: pg_sage_migration_enrich_dropped_total.
func EnrichDroppedCounts() []EnrichDropCount {
	enrichDrops.mu.Lock()
	out := make([]EnrichDropCount, 0, len(enrichDrops.byDB))
	for db, n := range enrichDrops.byDB {
		out = append(out, EnrichDropCount{Database: db, Count: n})
	}
	enrichDrops.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}
