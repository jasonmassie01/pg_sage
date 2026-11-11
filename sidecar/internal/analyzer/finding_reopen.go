package analyzer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// DetailPersistedAfterAction is the Detail key holding the action_log id
// of the action that resolved a finding whose condition then persisted:
// the action's row is reopened with it instead of a new row (rules-06).
const DetailPersistedAfterAction = "persisted_after_action"

// errUpsertRaced reports that a concurrent upserter opened the identity
// between this upsert's read and its write.
var errUpsertRaced = errors.New("identity opened concurrently")

// Evidence says when a batch of findings was observed.
type Evidence struct {
	// CollectedAt is when the snapshot the findings came from was taken.
	// Zero means unknown.
	CollectedAt time.Time
	// Window bounds how long after an action resolved a finding the
	// same condition reopens the action's row. Zero means the default,
	// reopenWindow(nil).
	Window time.Duration
}

func (e Evidence) window() time.Duration {
	if e.Window > 0 {
		return e.Window
	}
	return reopenWindow(nil)
}

// reopenWindow is 2 x max(analyzer interval, collector interval), with
// the defaults standing in for unset or invalid intervals: evidence of a
// condition that persists after an action reaches the analyzer within
// one collector and one analyzer cycle.
func reopenWindow(cfg *config.Config) time.Duration {
	analyzerIv := config.DefaultAnalyzerInterval
	collectorIv := config.DefaultCollectorInterval
	if cfg != nil && cfg.Analyzer.IntervalSeconds > 0 {
		analyzerIv = cfg.Analyzer.Interval()
	}
	if cfg != nil && cfg.Collector.IntervalSeconds > 0 {
		collectorIv = cfg.Collector.Interval()
	}
	return 2 * max(analyzerIv, collectorIv)
}

type afterAction int

const (
	afterActionInsert afterAction = iota // no relevant action: open a new row
	afterActionStale                     // evidence predates the action: skip
	afterActionReopen                    // condition persisted: reopen the row
)

// unknownEvidenceAge is how old evidence without a collection time is
// assumed to be: two default collector intervals. Only callers outside the
// analyzer cycle (runway, schema lint, audit, cloud telemetry) pass no
// evidence time; the analyzer passes its snapshot's CollectedAt.
const unknownEvidenceAge = 2 * config.DefaultCollectorInterval

// decideAfterAction classifies a finding whose identity was last resolved
// by an action at resolvedAt. Evidence collected at or before resolvedAt
// is stale. Evidence within window after it reopens the action's row; a
// later one is a new occurrence. Evidence with no collection time is
// assumed unknownEvidenceAge older than the database clock dbNow.
func decideAfterAction(
	resolvedAt, collectedAt, dbNow time.Time, window time.Duration,
) afterAction {
	if collectedAt.IsZero() {
		collectedAt = dbNow.Add(-unknownEvidenceAge)
	}
	if !collectedAt.After(resolvedAt) {
		return afterActionStale
	}
	if collectedAt.Sub(resolvedAt) <= window {
		return afterActionReopen
	}
	return afterActionInsert
}

// actionResolution is the latest row of an identity resolved by an action.
type actionResolution struct {
	id         int64
	actionID   int64
	resolvedAt time.Time
	dbNow      time.Time
}

// openOrReopen handles a finding with no open row: it reopens the row an
// action resolved when the condition persisted, skips stale evidence, or
// inserts a new open row.
func openOrReopen(
	ctx context.Context, pool *pgxpool.Pool, f Finding, ev Evidence,
) (upsertOutcome, error) {
	last, found, err := lastActionResolution(ctx, pool, f)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("look up action resolution: %w", err)
	}
	if found {
		switch decideAfterAction(last.resolvedAt, ev.CollectedAt, last.dbNow, ev.window()) {
		case afterActionStale:
			return outcomeSkipped, nil
		case afterActionReopen:
			return reopenAfterAction(ctx, pool, f, last)
		}
	}
	detailJSON, err := findingDetailJSON(f)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("marshal detail: %w", err)
	}
	if err := insertFinding(ctx, pool, f, detailJSON); err != nil {
		if errors.Is(err, errUpsertRaced) {
			return outcomeSkipped, err
		}
		return outcomeSkipped, fmt.Errorf("insert: %w", err)
	}
	return outcomeOpened, nil
}

func lastActionResolution(
	ctx context.Context, pool *pgxpool.Pool, f Finding,
) (actionResolution, bool, error) {
	var r actionResolution
	err := pool.QueryRow(ctx,
		`/* pg_sage analyzer */ SELECT id, action_log_id, resolved_at, now()
		   FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2
		    AND status = 'resolved'
		    AND action_log_id IS NOT NULL AND resolved_at IS NOT NULL
		  ORDER BY resolved_at DESC
		  LIMIT 1`,
		f.Category, f.ObjectIdentifier,
	).Scan(&r.id, &r.actionID, &r.resolvedAt, &r.dbNow)
	if errors.Is(err, pgx.ErrNoRows) {
		return actionResolution{}, false, nil
	}
	if err != nil {
		return actionResolution{}, false, err
	}
	return r, true, nil
}

// reopenAfterAction reopens the row last resolved by an action. The
// action link moves into the detail, and action_log_id, acted_on_at and
// resolved_at are cleared, so a later plain resolution of this row is
// not mistaken for another action resolution.
func reopenAfterAction(
	ctx context.Context, pool *pgxpool.Pool, f Finding, last actionResolution,
) (upsertOutcome, error) {
	detailJSON, err := detailWithPersistedAfterAction(f, last.actionID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("marshal detail: %w", err)
	}
	tag, err := pool.Exec(ctx,
		`/* pg_sage analyzer */ UPDATE sage.findings SET `+findingRefreshSet+`,
		     status = 'open', resolved_at = NULL, acted_on_at = NULL,
		     action_log_id = NULL
		 WHERE id = $9 AND status = 'resolved'`,
		append(refreshArgs(f, detailJSON), last.id)...,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// A concurrent upserter opened the identity (idx_findings_dedup).
		return outcomeSkipped, errUpsertRaced
	}
	if err != nil {
		return outcomeSkipped, fmt.Errorf("reopen row %d after action %d: %w",
			last.id, last.actionID, err)
	}
	if tag.RowsAffected() == 0 {
		return outcomeSkipped, errUpsertRaced
	}
	return outcomeReopened, nil
}

// detailWithPersistedAfterAction is f's stored detail plus the action
// link. The caller's map is copied, never written.
func detailWithPersistedAfterAction(f Finding, actionID int64) ([]byte, error) {
	detail := make(map[string]any, len(f.Detail)+1)
	for k, v := range f.Detail {
		detail[k] = v
	}
	detail[DetailPersistedAfterAction] = actionID
	f.Detail = detail
	return findingDetailJSON(f)
}
