package rca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors returned by ResolveIncident.
var (
	ErrIncidentNotFound        = errors.New("incident not found")
	ErrIncidentAlreadyResolved = errors.New("incident already resolved")
)

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-` +
		`[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const hydrateSQL = `/* pg_sage */
SELECT id::text, detected_at, COALESCE(last_detected_at, detected_at),
       severity, root_cause, causal_chain, affected_objects, signal_ids,
       COALESCE(recommended_sql, ''), COALESCE(rollback_sql, ''),
       COALESCE(action_risk, ''), source, confidence::float8,
       COALESCE(database_name, ''), occurrence_count, escalated_at,
       COALESCE(previous_incident_id::text, '')
FROM sage.incidents
WHERE resolved_at IS NULL
  AND (database_name = $1 OR database_name IS NULL OR database_name = '')
ORDER BY detected_at DESC
LIMIT $2`

const syncResolvedSQL = `/* pg_sage */
SELECT id::text, resolved_at, COALESCE(resolved_by, ''),
       COALESCE(resolution_reason, '')
FROM sage.incidents
WHERE id = ANY($1::uuid[]) AND resolved_at IS NOT NULL`

// Hydrate binds the engine to pool as its durable incident store and, on
// the first successful call, loads this database's open incidents so a
// restart keeps incident identity (R04 / substrate-B5). It also learns
// current_database() for log filtering when WithLogDatabase was not set.
// Later calls are no-ops.
func (e *Engine) Hydrate(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("rca: hydrate requires a pool")
	}
	e.cycleMu.Lock()
	defer e.cycleMu.Unlock()

	e.mu.Lock()
	done, name, logDB := e.hydrated, e.databaseName, e.logDatabase
	e.mu.Unlock()
	if done {
		return nil
	}
	if logDB == "" {
		if err := pool.QueryRow(ctx,
			"SELECT current_database()").Scan(&logDB); err != nil {
			return fmt.Errorf("rca: hydrate current_database: %w", err)
		}
	}
	e.reconcileLegacy(ctx, pool, name)
	loaded, err := loadOpenIncidents(ctx, pool, name)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mergeHydrated(loaded, name)
	e.logDatabase = logDB
	e.store = pool
	e.hydrated = true
	e.logFn("info", "rca: hydrated %d open incidents for %q",
		len(loaded), name)
	return nil
}

func (e *Engine) mergeHydrated(loaded []Incident, name string) {
	known := make(map[string]bool, len(e.incidents))
	for _, inc := range e.incidents {
		known[inc.ID] = true
	}
	for _, inc := range loaded {
		if known[inc.ID] {
			continue
		}
		if inc.DatabaseName == "" {
			inc.DatabaseName = name // adopt legacy rows
		}
		e.incidents = append(e.incidents, inc)
		e.trackFor(inc.ID).persisted = true
	}
}

func loadOpenIncidents(
	ctx context.Context, pool *pgxpool.Pool, name string,
) ([]Incident, error) {
	rows, err := pool.Query(ctx, hydrateSQL, name, maxTrackedIncidents)
	if err != nil {
		return nil, fmt.Errorf("rca: hydrate query: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		inc, err := scanHydratedIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rca: hydrate rows: %w", err)
	}
	return out, nil
}

func scanHydratedIncident(rows pgx.Rows) (Incident, error) {
	var inc Incident
	var chain []byte
	err := rows.Scan(&inc.ID, &inc.DetectedAt, &inc.LastDetectedAt,
		&inc.Severity, &inc.RootCause, &chain, &inc.AffectedObjects,
		&inc.SignalIDs, &inc.RecommendedSQL, &inc.RollbackSQL,
		&inc.ActionRisk, &inc.Source, &inc.Confidence,
		&inc.DatabaseName, &inc.OccurrenceCount, &inc.EscalatedAt,
		&inc.PreviousIncidentID)
	if err != nil {
		return inc, fmt.Errorf("rca: hydrate scan: %w", err)
	}
	if len(chain) > 0 {
		if err := json.Unmarshal(chain, &inc.CausalChain); err != nil {
			return inc, fmt.Errorf(
				"rca: hydrate causal_chain of %s: %w", inc.ID, err)
		}
	}
	return inc, nil
}

// syncResolved marks in-memory incidents resolved when the database says
// they were resolved (an operator used the API). This runs before dedup,
// so a recurrence in the same cycle becomes a new, linked incident rather
// than a silent reopen.
func (e *Engine) syncResolved(ctx context.Context) {
	e.mu.Lock()
	pool := e.store
	var ids []string
	for _, inc := range e.incidents {
		if inc.ResolvedAt == nil && e.trackFor(inc.ID).persisted {
			ids = append(ids, inc.ID)
		}
	}
	e.mu.Unlock()
	if pool == nil || len(ids) == 0 {
		return
	}
	resolved, err := queryResolved(ctx, pool, ids)
	if err != nil {
		e.logFn("warn", "rca: sync resolved incidents: %v", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.incidents {
		inc := &e.incidents[i]
		if r, ok := resolved[inc.ID]; ok && inc.ResolvedAt == nil {
			inc.ResolvedAt = r.ResolvedAt
			inc.ResolvedBy = r.ResolvedBy
			inc.ResolutionReason = r.ResolutionReason
			delete(e.clearCounts, inc.ID)
		}
	}
}

func queryResolved(
	ctx context.Context, pool *pgxpool.Pool, ids []string,
) (map[string]Incident, error) {
	rows, err := pool.Query(ctx, syncResolvedSQL, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Incident)
	for rows.Next() {
		var inc Incident
		if err := rows.Scan(&inc.ID, &inc.ResolvedAt, &inc.ResolvedBy,
			&inc.ResolutionReason); err != nil {
			return nil, err
		}
		out[inc.ID] = inc
	}
	return out, rows.Err()
}

// ResolveIncident is the operator resolution transition used by the API.
// It records who resolved the incident and why, and never overwrites an
// existing resolution (SURF-19).
func ResolveIncident(
	ctx context.Context, pool *pgxpool.Pool, id, resolvedBy, reason string,
) error {
	if resolvedBy == "" {
		return errors.New("rca: resolve requires a resolver identity")
	}
	if !uuidPattern.MatchString(id) {
		return ErrIncidentNotFound
	}
	tag, err := pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.incidents
		SET resolved_at = now(), resolved_by = $2,
		    resolution_reason = NULLIF($3, '')
		WHERE id = $1 AND resolved_at IS NULL`, id, resolvedBy, reason)
	if err != nil {
		return fmt.Errorf("rca: resolve incident: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var resolved bool
	err = pool.QueryRow(ctx, `/* pg_sage */
		SELECT resolved_at IS NOT NULL FROM sage.incidents WHERE id = $1`,
		id).Scan(&resolved)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIncidentNotFound
	}
	if err != nil {
		return fmt.Errorf("rca: resolve incident lookup: %w", err)
	}
	if resolved {
		return ErrIncidentAlreadyResolved
	}
	return fmt.Errorf("rca: resolve incident %s: no row updated", id)
}

const pruneBatchSize = 1000

// pruneIncidentsSQL deletes one batch of incidents resolved before the
// window, oldest first. The batch's ids are collected first (ARRAY), so the
// delete goes through the primary key; with id IN (subquery) the planner
// may hash-join a full scan of sage.incidents (perf gate).
const pruneIncidentsSQL = `/* pg_sage */
DELETE FROM sage.incidents WHERE id = ANY (ARRAY(
    SELECT id FROM sage.incidents
    WHERE resolved_at IS NOT NULL
      AND resolved_at < now() - make_interval(secs => $1)
    ORDER BY resolved_at LIMIT $2))`

// PruneResolvedIncidents deletes incidents resolved more than retention
// ago, in batches. Open incidents are never deleted; recurrence links to
// a pruned incident are cleared by the ON DELETE SET NULL foreign key.
// This is the retention hook for sage.incidents (substrate-B7); callers
// own the schedule. A non-positive retention is rejected.
func PruneResolvedIncidents(
	ctx context.Context, pool *pgxpool.Pool, retention time.Duration,
) (int64, error) {
	if retention <= 0 {
		return 0, fmt.Errorf("rca: prune retention must be positive, got %s",
			retention)
	}
	var total int64
	for {
		tag, err := pool.Exec(ctx, pruneIncidentsSQL, retention.Seconds(), pruneBatchSize)
		if err != nil {
			return total, fmt.Errorf("rca: prune resolved incidents: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < pruneBatchSize {
			return total, nil
		}
	}
}
