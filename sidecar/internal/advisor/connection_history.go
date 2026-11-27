package advisor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/histstore"
)

// connHistoryTimeout bounds the one history read of a connection review.
const connHistoryTimeout = 10 * time.Second

// connWindow is what the system snapshots of the advisor window say about
// connections (advisors-07): the peak of total backends (instance-wide) and
// the first reading of this database's pg_stat_database.sessions counter,
// which counts sessions that already closed. A field that is not known is
// never zero-valued into a fact: peakKnown false is "no peak", not 0.
type connWindow struct {
	window        time.Duration
	peakTotal     int
	peakKnown     bool
	firstSessions int64
	firstEpoch    time.Time
	firstAt       time.Time
	sessionsKnown bool
}

// connHistorySource reads the window from the history store.
type connHistorySource interface {
	readConnWindow(ctx context.Context, window time.Duration) (connWindow, error)
}

// connWindowSQL reads the window's system rows in one statement: the
// expression the forecaster uses for the peak (max of total_backends), and
// the earliest row carrying the sessions counter (PG 14+) with its
// statistics epoch.
const connWindowSQL = `/* pg_sage */
SELECT max((s.data->>'total_backends')::int),
       (array_agg((s.data->>'sessions')::bigint ORDER BY s.collected_at)
           FILTER (WHERE (s.data->>'sessions')::bigint > 0))[1],
       (array_agg((s.data->>'relation_stats_epoch')::timestamptz ORDER BY s.collected_at)
           FILTER (WHERE (s.data->>'sessions')::bigint > 0))[1],
       (array_agg(s.collected_at ORDER BY s.collected_at)
           FILTER (WHERE (s.data->>'sessions')::bigint > 0))[1]
  FROM sage.snapshots s
 WHERE {db:s} AND s.category = 'system'
   AND s.collected_at >= now() - make_interval(secs => $1)`

// pgConnHistory reads the window through the database's history store
// (histstore.Resolve: the meta database in history.store: meta).
type pgConnHistory struct{ db any }

func (p pgConnHistory) readConnWindow(ctx context.Context, window time.Duration) (
	connWindow, error) {
	var peak *int
	var first *int64
	var epoch, at *time.Time
	err := histstore.Resolve(p.db).QueryRow(ctx, connWindowSQL, window.Seconds()).
		Scan(&peak, &first, &epoch, &at)
	if err != nil {
		return connWindow{}, err
	}
	w := connWindow{window: window}
	if peak != nil {
		w.peakTotal, w.peakKnown = *peak, true
	}
	if first != nil && epoch != nil && at != nil {
		w.firstSessions, w.firstEpoch, w.firstAt = *first, epoch.UTC(), *at
		w.sessionsKnown = true
	}
	return w, nil
}

// connEnv is what the connection sub-advisor knows beyond the snapshot:
// the database it reviews and the window's history.
type connEnv struct {
	database string
	hist     connWindow
}

type connEnvKey struct{}

func withConnEnv(ctx context.Context, env connEnv) context.Context {
	return context.WithValue(ctx, connEnvKey{}, env)
}

// connEnvFrom is the review's connection environment; without one the
// database is unnamed and the window unknown.
func connEnvFrom(ctx context.Context) connEnv {
	env, _ := ctx.Value(connEnvKey{}).(connEnv)
	return env
}

// connectionContext reads the advisor window's history once for the
// connection sub-advisor.
func (a *Advisor) connectionContext(ctx context.Context) context.Context {
	return withConnEnv(ctx, readConnEnv(ctx, a.connHist, a.cfg.Advisor.Interval(),
		a.database(), a.logFn))
}

// readConnEnv reads the window of database's history. Without a source, or
// on a read error, the window is unknown: max_connections decreases stay
// advisory and churn is unknown. The error is logged with the database, so
// it is never read as a peak of 0.
func readConnEnv(ctx context.Context, src connHistorySource, window time.Duration,
	database string, logFn func(string, string, ...any)) connEnv {
	env := connEnv{database: database, hist: connWindow{window: window}}
	if src == nil || window <= 0 {
		logFn("DEBUG", "advisor: connections: database %q: no connection history to "+
			"read; the peak over the window is unknown", database)
		return env
	}
	rctx, cancel := context.WithTimeout(ctx, connHistoryTimeout)
	defer cancel()
	w, err := src.readConnWindow(rctx, window)
	if err != nil {
		logFn("WARN", "advisor: connections: database %q: reading the connection "+
			"history of the last %s failed, so the peak is unknown, max_connections "+
			"decreases stay advisory and churn is unknown: %v", database, window, err)
		return env
	}
	w.window = window
	env.hist = w
	return env
}

// connChurn is how many sessions this database opened over the window and
// the rate per minute, from the pg_stat_database.sessions delta (closed
// sessions included). It is unknown when the counter is not read (before
// PG 14, legacy rows), the statistics epoch moved, or the counter fell.
func connChurn(snap *collector.Snapshot, w connWindow) (opened int64, perMin float64,
	ok bool) {
	if snap == nil || !w.sessionsKnown {
		return 0, 0, false
	}
	cur := snap.System
	if cur.Sessions <= 0 || cur.Sessions < w.firstSessions ||
		!sameStatsEpoch(w.firstEpoch, cur.RelationStatsEpoch) {
		return 0, 0, false
	}
	minutes := snap.CollectedAt.Sub(w.firstAt).Minutes()
	if minutes <= 0 {
		return 0, 0, false
	}
	opened = cur.Sessions - w.firstSessions
	return opened, float64(opened) / minutes, true
}

// sameStatsEpoch reports two known, equal statistics epochs.
func sameStatsEpoch(a, b time.Time) bool {
	return !a.IsZero() && !b.IsZero() && a.Equal(b)
}
