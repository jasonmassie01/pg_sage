package receipt

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/verify"
)

// LockWindow is how far around landing lock harm counts against a change.
const LockWindow = 15 * time.Minute

// maxHarm bounds the incidents and lock findings one receipt names.
const maxHarm = 20

// ChangeFeed is a database's change feed (*changefeed.Feed): signed deploy
// and migration events anchor landings; pg_sage's own stats_reset, restart
// and failover events confound receipts.
type ChangeFeed interface {
	List(ctx context.Context, f changefeed.Filter) ([]changefeed.Event, error)
}

// Observer reads what a receipt rests on from the monitored database: the
// watched statements' history through histstore.Resolve(Pool) (the meta
// database with history.store: meta), incidents, lock_chain findings,
// pg_stat_statements text for born queryids, and the change feed. Changes
// nil means no feed: no confounders are known. Every read is cut off at
// ReadTimeout (zero: DefaultReadTimeout), so a slow history never holds
// the leader's cycle.
type Observer struct {
	Pool        *pgxpool.Pool
	Changes     ChangeFeed
	Logf        func(format string, args ...any)
	ReadTimeout time.Duration
}

// DefaultReadTimeout bounds one observation read.
const DefaultReadTimeout = 10 * time.Second

// readContext is ctx bounded by the read timeout.
func (o Observer) readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	d := o.ReadTimeout
	if d <= 0 {
		d = DefaultReadTimeout
	}
	return context.WithTimeout(ctx, d)
}

// Observe reads the observations of landed review r over w. A failed read
// is an error (the receipt waits for the next cycle), except the born
// queryids, which are informative: their failure is logged.
func (o Observer) Observe(ctx context.Context, r changereview.Review,
	w Windows) (Observations, error) {
	if o.Pool == nil {
		return Observations{}, fmt.Errorf("%w: the observer has no database",
			changereview.ErrInvalid)
	}
	var obs Observations
	var err error
	if obs.Before, obs.After, err = o.measurements(ctx, r, w); err != nil {
		return obs, err
	}
	if obs.Confounders, err = o.confounders(ctx, w); err != nil {
		return obs, unavailable("read the change feed", err)
	}
	names := relationNames(r.Expectations.Relations)
	lo, hi := w.BeforeTo.Add(-LockWindow), w.AfterFrom.Add(LockWindow)
	if obs.Incidents, err = o.incidents(ctx, names, lo, hi); err != nil {
		return obs, unavailable("read lock incidents", err)
	}
	if obs.LockFindings, err = o.lockFindings(ctx, names, lo, hi); err != nil {
		return obs, unavailable("read lock_chain findings", err)
	}
	if obs.Born, err = o.born(ctx, r, w); err != nil {
		o.logf("change review: receipt of %s: born queryids unavailable: %v", r.ID, err)
		obs.Born = nil
	}
	return obs, nil
}

// measurements reads the watched statements before and after landing.
func (o Observer) measurements(ctx context.Context, r changereview.Review,
	w Windows) (before, after map[int64]verify.Measurement, err error) {
	src := verify.NewPostgresObservationSource(o.Pool)
	ids := r.Expectations.WatchQueryIDs
	rctx, cancel := o.readContext(ctx)
	defer cancel()
	if before, err = src.QueryMeasurements(rctx, ids, w.BeforeFrom, w.BeforeTo); err != nil {
		return nil, nil, unavailable("read the watched statements before landing", err)
	}
	actx, cancelAfter := o.readContext(ctx)
	defer cancelAfter()
	if after, err = src.QueryMeasurements(actx, ids, w.AfterFrom, w.AfterTo); err != nil {
		return nil, nil, unavailable("read the watched statements after landing", err)
	}
	return before, after, nil
}

func unavailable(what string, err error) error {
	return fmt.Errorf("%w: %s: %v", changereview.ErrUnavailable, what, err)
}

func (o Observer) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// confounderKinds make a before/after comparison untrustworthy.
var confounderKinds = map[changefeed.Kind]bool{changefeed.KindStatsReset: true,
	changefeed.KindRestart: true, changefeed.KindFailover: true}

// confounders lists the confounding events inside w, oldest first.
func (o Observer) confounders(ctx context.Context, w Windows) ([]string, error) {
	if o.Changes == nil {
		return nil, nil
	}
	rctx, cancel := o.readContext(ctx)
	defer cancel()
	events, err := listEvents(rctx, o.Changes, w.BeforeFrom, w.AfterTo)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := len(events) - 1; i >= 0; i-- {
		if e := events[i]; confounderKinds[e.Kind] {
			out = append(out, fmt.Sprintf("%s at %s", e.Kind,
				e.OccurredAt.UTC().Format(time.RFC3339)))
		}
	}
	return out, nil
}

// maxEventPages bounds how many pages of the feed one read takes.
const maxEventPages = 10

// listEvents reads the feed's events in [since, until], newest first,
// paging backwards past the store's list limit.
func listEvents(ctx context.Context, feed ChangeFeed, since,
	until time.Time) ([]changefeed.Event, error) {
	var out []changefeed.Event
	seen := map[string]bool{}
	for page := 0; page < maxEventPages; page++ {
		batch, err := feed.List(ctx, changefeed.Filter{Since: since, Until: until,
			Limit: changefeed.MaxList})
		if err != nil {
			return nil, err
		}
		for _, e := range batch {
			if !seen[string(e.ID)] {
				seen[string(e.ID)] = true
				out = append(out, e)
			}
		}
		if len(batch) < changefeed.MaxList || !batch[len(batch)-1].OccurredAt.Before(until) {
			break
		}
		until = batch[len(batch)-1].OccurredAt
	}
	return out, nil
}

// relationNames are the names an incident or lock finding may give a
// touched relation: "schema.name", and the bare name in schema public
// (regclass text drops a schema on the search path).
func relationNames(rels []string) []string {
	out := make([]string, 0, 2*len(rels))
	for _, rel := range rels {
		rel = strings.ToLower(rel)
		out = append(out, rel)
		if name, ok := strings.CutPrefix(rel, "public."); ok {
			out = append(out, name)
		}
	}
	return out
}

const incidentsSQL = `/* pg_sage change_review_receipt_incidents v1 */
SELECT id::text FROM sage.incidents
WHERE detected_at <= $2 AND last_detected_at >= $1 AND affected_objects && $3::text[]
ORDER BY detected_at, id LIMIT $4`

// incidents are the incidents on names active in [lo, hi].
func (o Observer) incidents(ctx context.Context, names []string, lo,
	hi time.Time) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rctx, cancel := o.readContext(ctx)
	defer cancel()
	rows, err := o.Pool.Query(rctx, incidentsSQL, lo, hi, names, maxHarm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

const lockFindingsSQL = `/* pg_sage change_review_receipt_lock_findings v1 */
SELECT id FROM sage.findings
WHERE category = 'lock_chain' AND created_at <= $2 AND last_seen >= $1
  AND (detail->>'locked_relation' = ANY($3::text[])
       OR COALESCE(detail->'locked_relations', '[]'::jsonb) ?| $3::text[])
ORDER BY created_at, id LIMIT $4`

// lockFindings are the lock_chain findings on names seen in [lo, hi].
func (o Observer) lockFindings(ctx context.Context, names []string, lo,
	hi time.Time) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rctx, cancel := o.readContext(ctx)
	defer cancel()
	rows, err := o.Pool.Query(rctx, lockFindingsSQL, lo, hi, names, maxHarm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
