package querystore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// KeyframeInterval is how often a sampled query is written although its
// counters did not move, so a reader finds a sample of every observed
// query within AnchorLookback before any instant.
const KeyframeInterval = time.Hour

// AnchorLookback bounds how far before a window readers look for the
// sample the window starts from. A query sampled every cycle has a row at
// least every KeyframeInterval; an older last row means it left the
// sampled set, and its counters at the window start are unknown.
const AnchorLookback = 2 * KeyframeInterval

// Execer runs a statement (a pool, connection or transaction).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// recordSamplesSQL inserts one cycle's samples in one statement and stamps
// each with the fingerprint of the latest plan captured for its queryid
// (plan_hash, M0; NULL until a plan is fingerprinted).
const recordSamplesSQL = `/* pg_sage */ INSERT INTO sage.query_store
	   (queryid, calls, total_exec_time, mean_exec_time, rows, stats_epoch, plan_hash)
	 SELECT u.queryid, u.calls, u.total, u.mean, u.rows, u.epoch, p.plan_hash
	 FROM unnest($1::int8[], $2::int8[], $3::float8[], $4::float8[], $5::int8[],
	             $6::timestamptz[]) AS u(queryid, calls, total, mean, rows, epoch)
	 LEFT JOIN LATERAL (
	     SELECT e.plan_hash FROM sage.explain_cache e
	     WHERE e.queryid = u.queryid AND e.plan_hash IS NOT NULL
	     ORDER BY e.captured_at DESC, e.id DESC LIMIT 1) p ON true`

// insertSamples writes samples with one statement.
func insertSamples(ctx context.Context, db Execer, samples []Sample) error {
	n := len(samples)
	ids, calls, rows := make([]int64, n), make([]int64, n), make([]int64, n)
	totals, means := make([]float64, n), make([]float64, n)
	epochs := make([]*time.Time, n)
	for i, s := range samples {
		ids[i], calls[i], rows[i] = s.QueryID, s.Calls, s.Rows
		totals[i], means[i] = s.TotalExecMs, s.MeanExecMs
		epochs[i] = epochParam(s.StatsEpoch)
	}
	_, err := db.Exec(ctx, recordSamplesSQL, ids, calls, totals, means, rows, epochs)
	return err
}

// written is what the Recorder last wrote for a queryid.
type written struct {
	calls int64
	total float64
	epoch time.Time
	at    time.Time // when it was written
	seen  time.Time // when the query was last sampled
}

// Recorder writes a query's sample only when its counters moved since the
// last sample it wrote (calls, total time or statistics epoch), or when
// KeyframeInterval has passed (perf M11: lifeos wrote 290 queries a minute,
// changed or not, 16k rows an hour). Readers take an absent sample as
// "unchanged since the last one". Safe for concurrent use.
type Recorder struct {
	mu   sync.Mutex
	last map[int64]written
}

// NewRecorder returns a Recorder that has written nothing: its first cycle
// writes every sample.
func NewRecorder() *Recorder { return &Recorder{last: map[int64]written{}} }

// Record writes the samples due at now in one statement and returns how
// many it wrote. A failed write is retried on the next cycle.
func (r *Recorder) Record(ctx context.Context, db Execer, samples []Sample,
	now time.Time) (int, error) {
	samples = aggregateSamples(samples)
	if len(samples) == 0 {
		return 0, nil
	}
	if db == nil {
		return 0, errors.New("query_store: no database to record samples in")
	}
	if now.IsZero() {
		return 0, errors.New("query_store: zero sample time")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	due := r.due(samples, now)
	r.forget(now)
	if len(due) == 0 {
		return 0, nil
	}
	if err := insertSamples(ctx, db, due); err != nil {
		return 0, fmt.Errorf("query_store: record %d samples: %w", len(due), err)
	}
	for _, s := range due {
		r.last[s.QueryID] = written{calls: s.Calls, total: s.TotalExecMs,
			epoch: s.StatsEpoch, at: now, seen: now}
	}
	return len(due), nil
}

// due returns the samples to write at now and marks every sample seen.
func (r *Recorder) due(samples []Sample, now time.Time) []Sample {
	var out []Sample
	for _, s := range samples {
		w, ok := r.last[s.QueryID]
		if !ok || w.calls != s.Calls || w.total != s.TotalExecMs ||
			!w.epoch.Equal(s.StatsEpoch) || now.Sub(w.at) >= KeyframeInterval {
			out = append(out, s)
		}
		if ok {
			w.seen = now
			r.last[s.QueryID] = w
		}
	}
	return out
}

// forget drops queries not sampled for two keyframe intervals: they left
// pg_stat_statements' top statements, and memory stays bounded by it.
func (r *Recorder) forget(now time.Time) {
	for id, w := range r.last {
		if now.Sub(w.seen) > 2*KeyframeInterval {
			delete(r.last, id)
		}
	}
}

// tracked is how many queryids the Recorder remembers.
func (r *Recorder) tracked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.last)
}
