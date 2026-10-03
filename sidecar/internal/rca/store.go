package rca

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Persistence: the engine writes sage.incidents with compare-and-swap
// semantics. A row resolved in the database (by an operator through the
// API) is never updated again, so a manual resolution is durable (R04).
// ---------------------------------------------------------------------------

const insertIncidentSQL = `/* pg_sage */
INSERT INTO sage.incidents (
    id, detected_at, last_detected_at, severity, root_cause,
    causal_chain, affected_objects, signal_ids, recommended_sql,
    rollback_sql, action_risk, source, confidence, resolved_at,
    database_name, occurrence_count, escalated_at, identity_key,
    resolved_by, resolution_reason, previous_incident_id
) VALUES (
    $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
    NULLIF($19, ''), NULLIF($20, ''),
    COALESCE(NULLIF($21, '')::uuid, (
        SELECT p.id FROM sage.incidents p
        WHERE p.identity_key = $18 AND p.resolved_at IS NOT NULL
          AND p.id <> $1
        ORDER BY p.resolved_at DESC LIMIT 1))
)
ON CONFLICT (id) DO NOTHING
RETURNING COALESCE(previous_incident_id::text, '')`

const updateIncidentSQL = `/* pg_sage */
UPDATE sage.incidents SET
    last_detected_at  = $2,
    severity          = $3,
    root_cause        = $4,
    -- An unchanged chain keeps its TOAST pointer: no new chunks are written.
    causal_chain      = CASE WHEN causal_chain IS DISTINCT FROM $5::jsonb
                             THEN $5::jsonb ELSE causal_chain END,
    occurrence_count  = $6,
    escalated_at      = $7,
    identity_key      = $8,
    database_name     = COALESCE(NULLIF(database_name, ''), $9),
    resolved_at       = $10,
    resolved_by       = NULLIF($11, ''),
    resolution_reason = NULLIF($12, '')
WHERE id = $1 AND resolved_at IS NULL`

const readResolutionSQL = `/* pg_sage */
SELECT resolved_at, COALESCE(resolved_by, ''),
       COALESCE(resolution_reason, '')
FROM sage.incidents WHERE id = $1`

// persistItem is one incident to write, captured under e.mu.
type persistItem struct {
	inc       Incident
	persisted bool
	written   string // fingerprint of the stored row; "" unknown
}

// persistResult is what the database said about one incident.
type persistResult struct {
	id         string
	inserted   bool
	previousID string
	resolved   bool // resolution is durable in the database
	external   *Incident
	gone       bool
	// fingerprint of what this pass wrote; "" when nothing was written.
	fingerprint string
}

// PersistIncidents writes every tracked incident, then applies what the
// database reports back: rows resolved externally are adopted, durable
// resolutions leave memory, and pending notifications are dispatched.
func (e *Engine) PersistIncidents(
	ctx context.Context,
	pool *pgxpool.Pool,
) error {
	e.cycleMu.Lock()
	defer e.cycleMu.Unlock()

	items := e.persistSnapshot()
	results := make([]persistResult, 0, len(items))
	var errs []error
	var unchanged []string
	for _, it := range items {
		if it.unchanged() {
			unchanged = append(unchanged, it.inc.ID)
			continue
		}
		r, err := persistOne(ctx, pool, it)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"rca: persist incident %s: %w", it.inc.ID, err))
			continue
		}
		if !r.gone && r.external == nil {
			r.fingerprint = storedFingerprint(&it.inc)
		}
		results = append(results, r)
	}
	stored, err := checkUnchanged(ctx, pool, unchanged)
	if err != nil {
		errs = append(errs, err)
	}
	results = append(results, stored...)
	pending, d := e.applyPersistResults(results)
	if d != nil && len(pending) > 0 {
		e.dispatchEvents(ctx, d, e.decorateEvents(ctx, pending))
	}
	return errors.Join(errs...)
}

// persistSnapshot copies tracked incidents, resolved ones first so a
// superseded incident is resolved before its recurrence links to it.
func (e *Engine) persistSnapshot() []persistItem {
	e.mu.Lock()
	defer e.mu.Unlock()
	items := make([]persistItem, 0, len(e.incidents))
	for pass := 0; pass < 2; pass++ {
		for _, inc := range e.incidents {
			if (inc.ResolvedAt != nil) != (pass == 0) {
				continue
			}
			ts := e.trackFor(inc.ID)
			items = append(items, persistItem{
				inc: inc, persisted: ts.persisted, written: ts.written,
			})
		}
	}
	return items
}

func persistOne(
	ctx context.Context, pool *pgxpool.Pool, it persistItem,
) (persistResult, error) {
	res := persistResult{id: it.inc.ID}
	if !it.persisted {
		prev, inserted, err := insertIncident(ctx, pool, &it.inc)
		if err != nil {
			return res, err
		}
		if inserted {
			res.inserted = true
			res.previousID = prev
			res.resolved = it.inc.ResolvedAt != nil
			return res, nil
		}
	}
	return updateIncident(ctx, pool, &it.inc, res)
}

func insertIncident(
	ctx context.Context, pool *pgxpool.Pool, inc *Incident,
) (string, bool, error) {
	chain, err := marshalChain(inc.CausalChain)
	if err != nil {
		return "", false, err
	}
	var prev string
	err = pool.QueryRow(ctx, insertIncidentSQL,
		inc.ID, inc.DetectedAt, inc.LastDetectedAt,
		inc.Severity, inc.RootCause,
		chain, nonNil(inc.AffectedObjects),
		nonNil(inc.SignalIDs), inc.RecommendedSQL, inc.RollbackSQL,
		nilIfEmpty(inc.ActionRisk), inc.Source, inc.Confidence,
		inc.ResolvedAt, inc.DatabaseName, inc.OccurrenceCount,
		inc.EscalatedAt, identityKey(inc), inc.ResolvedBy,
		inc.ResolutionReason, inc.PreviousIncidentID,
	).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil // row already exists: update it instead
	}
	if err != nil {
		return "", false, fmt.Errorf("insert: %w", err)
	}
	return prev, true, nil
}

func updateIncident(
	ctx context.Context, pool *pgxpool.Pool, inc *Incident,
	res persistResult,
) (persistResult, error) {
	chain, err := marshalChain(inc.CausalChain)
	if err != nil {
		return res, err
	}
	tag, err := pool.Exec(ctx, updateIncidentSQL,
		inc.ID, inc.LastDetectedAt, inc.Severity, inc.RootCause,
		chain, inc.OccurrenceCount,
		inc.EscalatedAt, identityKey(inc), inc.DatabaseName,
		inc.ResolvedAt, inc.ResolvedBy, inc.ResolutionReason,
	)
	if err != nil {
		return res, fmt.Errorf("update: %w", err)
	}
	if tag.RowsAffected() == 1 {
		res.resolved = inc.ResolvedAt != nil
		return res, nil
	}
	// The row was resolved (or removed) outside the engine.
	ext := Incident{}
	err = pool.QueryRow(ctx, readResolutionSQL, inc.ID).Scan(
		&ext.ResolvedAt, &ext.ResolvedBy, &ext.ResolutionReason)
	if errors.Is(err, pgx.ErrNoRows) {
		res.gone = true
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("read resolution: %w", err)
	}
	if ext.ResolvedAt == nil {
		return res, fmt.Errorf("update matched no open row")
	}
	res.resolved = true
	res.external = &ext
	return res, nil
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
