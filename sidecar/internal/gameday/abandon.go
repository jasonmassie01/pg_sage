package gameday

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Abandoned runs (benches-03). A process that stops mid-run (a crash,
// kill -9, a redeploy) leaves its run "running" and its clone alive. A run
// still running MaxRunTime after it started cannot be live (no game day
// or local bench run takes that long), so it is marked abandoned and its
// recorded clone destroyed. A younger running row may belong to a live
// run of another process and is left alone.

// MaxRunTime is how long a run may stay running before it is abandoned.
const MaxRunTime = 6 * time.Hour

// runBudget bounds each run's clone and fault programs, so no live run
// reaches MaxRunTime and has its clone destroyed by another process's
// sweep. The margin covers what still runs after the budget ends: the
// in-flight scenario's recovery, the clone's destroy and the result.
const runBudget = MaxRunTime - time.Hour

// StatusAbandoned is a run its process never finished.
const StatusAbandoned = "abandoned"

// abandonedError is the error an abandoned run is recorded with.
var abandonedError = fmt.Sprintf("abandoned: still running %s after it started "+
	"(its process stopped mid-run)", MaxRunTime)

// staleRun is an abandoned run and the clone it recorded.
type staleRun struct{ id, cloneID string }

// abandonStale marks database's kind runs that started before cutoff and
// are still running as abandoned at at, and returns them. The update
// claims each row once, so two sweeps never destroy the same clone.
func (s *Store) abandonStale(ctx context.Context, database, kind string, cutoff,
	at time.Time) ([]staleRun, error) {
	rows, err := s.pool.Query(ctx, `UPDATE sage.sre_game_days
		SET status = 'abandoned', finished_at = $5, error = $6
		WHERE deployment_id = $1 AND database_name = $2 AND kind = $3
		  AND status = 'running' AND started_at < $4
		RETURNING id::text, COALESCE(clone_id, '')`, s.deployment, database, kind, cutoff,
		at, abandonedError)
	if err != nil {
		return nil, fmt.Errorf("abandon stale %s runs of %s: %w", kind, database, err)
	}
	defer rows.Close()
	var out []staleRun
	for rows.Next() {
		var r staleRun
		if err := rows.Scan(&r.id, &r.cloneID); err != nil {
			return nil, fmt.Errorf("abandon stale %s runs of %s: %w", kind, database, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("abandon stale %s runs of %s: %w", kind, database, err)
	}
	return out, nil
}

// noteError appends msg to a run's recorded error.
func (s *Store) noteError(ctx context.Context, id, msg string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sage.sre_game_days
		SET error = left(concat_ws('; ', error, $3::text), 2000)
		WHERE deployment_id = $1 AND id = $2`, s.deployment, id, msg)
	if err != nil {
		return fmt.Errorf("record the destroy error of run %s: %w", id, err)
	}
	return nil
}

// sweepStale abandons database's stale kind runs and destroys their
// clones. A failed Destroy is recorded on its run and the sweep goes on;
// the errors are returned joined, with the number of runs abandoned.
func sweepStale(ctx context.Context, store *Store, provider clone.Provider, database,
	kind string, now time.Time) (int, error) {
	stale, err := store.abandonStale(ctx, database, kind, now.Add(-MaxRunTime), now)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, r := range stale {
		if r.cloneID == "" {
			continue
		}
		derr := provider.Destroy(ctx, clone.Clone{ID: r.cloneID})
		if derr == nil {
			continue
		}
		msg := truncate(fmt.Sprintf("destroy clone %s: %v", r.cloneID, derr), 1000)
		errs = append(errs, fmt.Errorf("abandoned %s %s: %s", kind, r.id, msg),
			store.noteError(ctx, r.id, msg))
	}
	return len(stale), errors.Join(errs...)
}

// AbandonStale abandons this database's stale game days and destroys
// their clones. While this runner's own game day runs it does nothing:
// it cannot tell its live run from another, and the next check sweeps.
func (r *Runner) AbandonStale(ctx context.Context) (int, error) {
	if r.running.Load() {
		return 0, nil
	}
	return sweepStale(ctx, r.store, r.provider, r.cfg.Database, rowGameDay, r.now())
}

// AbandonStale abandons this database's stale local bench runs and
// destroys their clones; without a store there are none.
func (b *LocalBench) AbandonStale(ctx context.Context) (int, error) {
	if b.cfg.Store == nil || b.running.Load() {
		return 0, nil
	}
	return b.sweep(ctx)
}

func (b *LocalBench) sweep(ctx context.Context) (int, error) {
	return sweepStale(ctx, b.cfg.Store, b.provider, b.cfg.Database, rowLocal,
		b.cfg.Now().UTC())
}
