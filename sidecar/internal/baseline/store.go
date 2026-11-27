package baseline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps the buckets in sage.baseline_samples: one row per
// (database, metric, key, week, bucket) holding the mean of the cycles
// folded into it and their count. Rows are written only to pg_sage's own
// schema; nothing is read from the monitored workload.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore keeps the buckets in pool's sage schema.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// storeTimeout bounds one read or write of the buckets.
const storeTimeout = 10 * time.Second

var errNoPool = errors.New("baseline store has no database")

// accumulateSQL folds each sample into its bucket's running mean. A
// concurrent fold of the same row serializes on the row lock.
const accumulateSQL = `INSERT INTO sage.baseline_samples AS b
	(database, metric, key, how, week_start, bucket, n, value, updated_at)
SELECT $1, u.metric, u.key, u.how, $2::date, $3::smallint, 1, u.value, now()
  FROM unnest($4::text[], $5::text[], $6::text[], $7::float8[])
       AS u(metric, key, how, value)
ON CONFLICT (database, metric, key, week_start, bucket) DO UPDATE
   SET n = b.n + 1,
       value = b.value + (EXCLUDED.value - b.value) / (b.n + 1),
       how = EXCLUDED.how,
       updated_at = now()`

// Accumulate folds samples, taken at at, into their bucket of at's week
// for database. Invalid samples are dropped (Clean); none is a no-op.
func (s *Store) Accumulate(ctx context.Context, database string, w Window, at time.Time,
	samples []Sample) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("accumulate sage.baseline_samples: %w", errNoPool)
	}
	samples = Clean(samples)
	if len(samples) == 0 {
		return nil
	}
	metrics := make([]string, len(samples))
	keys := make([]string, len(samples))
	hows := make([]string, len(samples))
	values := make([]float64, len(samples))
	for i, smp := range samples {
		metrics[i], keys[i], hows[i], values[i] = smp.Key.Metric, smp.Key.Key, smp.How,
			smp.Value
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if _, err := s.pool.Exec(ctx, accumulateSQL, database, w.WeekStart(at), w.Bucket(at),
		metrics, keys, hows, values); err != nil {
		return fmt.Errorf("accumulate %d samples into sage.baseline_samples for %q: %w",
			len(samples), database, err)
	}
	return nil
}

// historySQL reads the bucket's values of the earlier weeks of the
// window, oldest first, for the (metric, key) pairs asked for.
const historySQL = `SELECT b.metric, b.key, b.value
  FROM sage.baseline_samples b
  JOIN unnest($5::text[], $6::text[]) AS w(metric, key)
    ON w.metric = b.metric AND w.key = b.key
 WHERE b.database = $1 AND b.bucket = $2::smallint
   AND b.week_start < $3::date AND b.week_start >= $3::date - $4::int
 ORDER BY b.metric, b.key, b.week_start`

// History returns, per key, at's bucket in each of the window's earlier
// weeks that has data, oldest first; never at's own week.
func (s *Store) History(ctx context.Context, database string, w Window, at time.Time,
	keys []Key) (map[Key][]float64, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("read sage.baseline_samples: %w", errNoPool)
	}
	out := make(map[Key][]float64, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	metrics := make([]string, len(keys))
	names := make([]string, len(keys))
	for i, k := range keys {
		metrics[i], names[i] = k.Metric, k.Key
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, historySQL, database, w.Bucket(at), w.WeekStart(at),
		7*w.weeks(), metrics, names)
	if err != nil {
		return nil, fmt.Errorf("read sage.baseline_samples for %q: %w", database, err)
	}
	defer rows.Close()
	for rows.Next() {
		var k Key
		var v float64
		if err := rows.Scan(&k.Metric, &k.Key, &v); err != nil {
			return nil, fmt.Errorf("scan sage.baseline_samples: %w", err)
		}
		out[k] = append(out[k], v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sage.baseline_samples for %q: %w", database, err)
	}
	return out, nil
}
