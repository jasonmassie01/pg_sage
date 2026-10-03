package forecaster

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// The query volume and sequence forecasts read each day's first and last
// non-empty snapshot of a category (dogfood lifeos-1). v1.8.3
// (performance gate offender 4): the samples are found by index, one
// probe per day, instead of ranking every snapshot of the lookback, and a
// Forecaster decodes each sample once (a delta row is rebuilt in
// PL/pgSQL) and remembers it: a run decodes only the samples it has not
// seen, normally today's newest. The daily aggregation is computed in Go
// from the decoded samples, exactly as the SQL did.

// dayDecodeBatch bounds the snapshots one statement decodes.
const dayDecodeBatch = 16

// dayPicksSQL returns, per day of the lookback (session time zone), the
// first and the last non-empty snapshot of category $1.
var dayPicksSQL = `/* pg_sage */
SELECT d.day, f.id, f.collected_at, l.id, l.collected_at
  FROM generate_series(date_trunc('day', now() - make_interval(days => $2)), now(),
                       interval '1 day') AS d(day)
 CROSS JOIN LATERAL (
       SELECT s.id, s.collected_at FROM sage.snapshots s
        WHERE s.category = $1 AND s.collected_at >= d.day
          AND s.collected_at < d.day + interval '1 day'
          AND s.collected_at > now() - make_interval(days => $2)
          AND ` + snapstore.NonEmptySQL("s") + `
        ORDER BY s.collected_at, s.id LIMIT 1) f
 CROSS JOIN LATERAL (
       SELECT s.id, s.collected_at FROM sage.snapshots s
        WHERE s.category = $1 AND s.collected_at >= d.day
          AND s.collected_at < d.day + interval '1 day'
          AND s.collected_at > now() - make_interval(days => $2)
          AND ` + snapstore.NonEmptySQL("s") + `
        ORDER BY s.collected_at DESC, s.id DESC LIMIT 1) l
 ORDER BY d.day`

// decodedDocsSQL rebuilds the snapshots $1 through the snapshot accessor
// once each; %s selects from d (id, doc).
const decodedDocsSQL = `/* pg_sage */
WITH d AS MATERIALIZED (
    SELECT s.id, %s AS doc FROM sage.snapshots s WHERE s.id = ANY($1::int8[])
)
%s`

// queryCallsSQL reads each sample's (queryid, calls).
var queryCallsSQL = fmt.Sprintf(decodedDocsSQL, snapstore.DataSQL("s"), `
SELECT d.id, (elem->>'queryid')::bigint, (elem->>'calls')::bigint
  FROM d, jsonb_array_elements(d.doc) AS elem`)

// sequenceUseSQL reads each sample's sequences at or over 1% used: none
// under it can be within the forecaster's horizons (the collector keeps
// those only as its top N).
var sequenceUseSQL = fmt.Sprintf(decodedDocsSQL, snapstore.DataSQL("s"), `
SELECT d.id, (elem->>'schemaname') || '.' || (elem->>'sequencename'),
       (elem->>'pct_used')::float, (elem->>'max_value')::bigint
  FROM d, jsonb_path_query(d.doc, '$[*] ? (@.pct_used >= 1)') AS elem`)

// daySample is one picked snapshot.
type daySample struct {
	day time.Time
	id  int64
	at  time.Time
}

// queryCalls is one element of a 'queries' sample.
type queryCalls struct {
	qid, calls *int64
}

// sequenceUse is one element of a 'sequences' sample.
type sequenceUse struct {
	name     string
	pct      float64
	maxValue int64
}

// dayHistory remembers the decoded samples of the current lookback.
type dayHistory struct {
	mu      sync.Mutex
	queries map[int64][]queryCalls
	seqs    map[int64][]sequenceUse
}

// dayPicks returns the lookback's first and last samples per day.
func dayPicks(ctx context.Context, pool *pgxpool.Pool, category string,
	days int) (first, last []daySample, err error) {
	rows, err := pool.Query(ctx, dayPicksSQL, category, days)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f, l daySample
		if err := rows.Scan(&f.day, &f.id, &f.at, &l.id, &l.at); err != nil {
			return nil, nil, fmt.Errorf("scan %s day samples: %w", category, err)
		}
		l.day = f.day
		first, last = append(first, f), append(last, l)
	}
	return first, last, rows.Err()
}

// missingIDs lists the samples not in cached, each once.
func missingIDs[T any](samples []daySample, cached map[int64][]T) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, s := range samples {
		if _, ok := cached[s.id]; !ok && !seen[s.id] {
			seen[s.id] = true
			out = append(out, s.id)
		}
	}
	return out
}

// decodeBatches runs decode over ids in bounded batches.
func decodeBatches(ids []int64, decode func([]int64) error) error {
	for start := 0; start < len(ids); start += dayDecodeBatch {
		if err := decode(ids[start:min(start+dayDecodeBatch, len(ids))]); err != nil {
			return err
		}
	}
	return nil
}

// keepOnly drops the cached samples no longer picked.
func keepOnly[T any](samples []daySample, cached map[int64][]T) map[int64][]T {
	out := make(map[int64][]T, len(samples))
	for _, s := range samples {
		if v, ok := cached[s.id]; ok {
			out[s.id] = v
		}
	}
	return out
}

// sortSamples orders samples by time, then id (the SQL's lag order).
func sortSamples(samples []daySample) {
	sort.SliceStable(samples, func(i, j int) bool {
		if !samples[i].at.Equal(samples[j].at) {
			return samples[i].at.Before(samples[j].at)
		}
		return samples[i].id < samples[j].id
	})
}
