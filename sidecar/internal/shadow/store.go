package shadow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Store is one monitored database's shadow ledger (sage.shadow_decision).
type Store struct{ pool *pgxpool.Pool }

// NewStore binds the shadow ledger of the database behind pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var classPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// seenSQL notes another sighting of a pending decision of the
// fingerprint and reports whether a new decision must not be recorded:
// one is pending, or one was recorded inside the dedupe window.
const seenSQL = `/* pg_sage */ WITH bump AS (
	UPDATE sage.shadow_decision SET last_seen_at = now(), seen_count = seen_count + 1
	 WHERE COALESCE(database_id, 0) = COALESCE($1::int, 0) AND fingerprint = $2
	   AND status = 'pending'
	RETURNING id)
SELECT EXISTS (SELECT 1 FROM bump) OR EXISTS (SELECT 1 FROM sage.shadow_decision
	WHERE COALESCE(database_id, 0) = COALESCE($1::int, 0) AND fingerprint = $2
	  AND recorded_at > now() - make_interval(secs => $3::double precision))`

// Seen reports whether the fingerprint already has a decision for this
// window (and bumps a pending one). It is the cheap check a cycle makes
// before it computes a prediction.
func (s *Store) Seen(ctx context.Context, databaseID *int, fingerprint string,
	window time.Duration) (bool, error) {
	if s == nil || s.pool == nil {
		return false, ErrUnavailable
	}
	var seen bool
	if err := s.pool.QueryRow(ctx, seenSQL, databaseID, fingerprint,
		window.Seconds()).Scan(&seen); err != nil {
		return false, fmt.Errorf("shadow: check fingerprint: %w", err)
	}
	return seen, nil
}

// recordSQL inserts a pending decision; when another cycle recorded the
// same fingerprint first, it bumps that one instead.
const recordSQL = `/* pg_sage */ INSERT INTO sage.shadow_decision (database_id,
	database_name, fingerprint, family, action_class, finding_id, recommendation_id,
	title, object_identifier, sql, rollback_sql, shape, prediction, evidence,
	gate_verdict, gate_reason, trusted_verdict, trusted_reason, trusted_detail,
	granted_level, decision_id)
VALUES ($1, $2, $3, $4, $5, NULLIF($6::bigint, 0), NULLIF($7::bigint, 0), $8, $9, $10,
	$11, $12, $13, $14, $15, $16, $17, $18, $19, $20, NULLIF($21::bigint, 0))
ON CONFLICT ((COALESCE(database_id, 0)), fingerprint) WHERE status = 'pending'
DO UPDATE SET last_seen_at = now(), seen_count = shadow_decision.seen_count + 1
RETURNING id, recorded_at, last_seen_at, seen_count, (xmax = 0)`

// Record stores d as a pending decision. inserted is false when another
// cycle recorded the same fingerprint at the same time (it was bumped).
func (s *Store) Record(ctx context.Context, d Decision) (Decision, bool, error) {
	if s == nil || s.pool == nil {
		return Decision{}, false, ErrUnavailable
	}
	if err := d.validate(); err != nil {
		return Decision{}, false, err
	}
	if d.Evidence == nil {
		d.Evidence = map[string]any{}
	}
	var inserted bool
	err := s.pool.QueryRow(ctx, recordSQL, d.DatabaseID, d.Database, d.Fingerprint,
		d.Family, d.Class, d.FindingID, d.RecommendationID, d.Title, d.Object, d.SQL,
		d.RollbackSQL, d.Shape, d.Prediction, d.Evidence, d.GateVerdict, d.GateReason,
		d.TrustedVerdict, d.TrustedReason, d.TrustedDetail, int16(d.GrantedLevel),
		d.DecisionID).Scan(&d.ID, &d.RecordedAt, &d.LastSeenAt, &d.SeenCount, &inserted)
	if err != nil {
		return Decision{}, false, fmt.Errorf("shadow: record %s decision: %w", d.Class, err)
	}
	d.Status = StatusPending
	if inserted {
		countDecision(d.Database, d.Class, d.TrustedVerdict)
	}
	return d, inserted, nil
}

const decisionColumns = `id, database_id, database_name, fingerprint, family,
	action_class, COALESCE(finding_id, 0), COALESCE(recommendation_id, 0), title,
	object_identifier, sql, rollback_sql, shape, prediction, evidence, gate_verdict,
	gate_reason, trusted_verdict, trusted_reason, trusted_detail, granted_level,
	COALESCE(decision_id, 0), status, COALESCE(score, ''), COALESCE(score_source, ''),
	counted, score_reason, score_detail, COALESCE(ref_action_log_id, 0),
	COALESCE(ref_queue_id, 0), applied_after, applied_detected_at, seen_count,
	recorded_at, last_seen_at, scored_at`

// scanDecision scans decisionColumns, then extra columns into extra.
func scanDecision(row pgx.Row, extra ...any) (Decision, error) {
	var d Decision
	var level int16
	dest := []any{&d.ID, &d.DatabaseID, &d.Database, &d.Fingerprint, &d.Family,
		&d.Class, &d.FindingID, &d.RecommendationID, &d.Title, &d.Object, &d.SQL,
		&d.RollbackSQL, &d.Shape, &d.Prediction, &d.Evidence, &d.GateVerdict,
		&d.GateReason, &d.TrustedVerdict, &d.TrustedReason, &d.TrustedDetail, &level,
		&d.DecisionID, &d.Status, &d.Score, &d.ScoreSource, &d.Counted, &d.ScoreReason,
		&d.ScoreDetail, &d.RefActionLogID, &d.RefQueueID, &d.AppliedAfter,
		&d.AppliedDetectedAt, &d.SeenCount, &d.RecordedAt, &d.LastSeenAt, &d.ScoredAt}
	err := row.Scan(append(dest, extra...)...)
	d.GrantedLevel = int(level)
	return d, err
}

// Get reads one decision.
func (s *Store) Get(ctx context.Context, id int64) (Decision, error) {
	if s == nil || s.pool == nil {
		return Decision{}, ErrUnavailable
	}
	d, err := scanDecision(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+decisionColumns+`
		FROM sage.shadow_decision WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	if err != nil {
		return Decision{}, fmt.Errorf("shadow: read decision %d: %w", id, err)
	}
	return d, nil
}

// Filter narrows a list: class, status and score ("" for any), and at
// most Limit decisions (1..1000, 0: 100), newest first.
type Filter struct {
	Class  string
	Status string
	Score  string
	Limit  int
}

func (f Filter) normalized() (Filter, error) {
	switch {
	case f.Class != "" && !classPattern.MatchString(f.Class):
		return f, fmt.Errorf("%w: class %q", ErrInvalid, f.Class)
	case f.Status != "" && !oneOf(f.Status, statuses):
		return f, fmt.Errorf("%w: status must be pending or scored", ErrInvalid)
	case f.Score != "" && !oneOf(f.Score, scores):
		return f, fmt.Errorf("%w: score must be correct, incorrect, neutral or unscored",
			ErrInvalid)
	case f.Limit < 0 || f.Limit > 1000:
		return f, fmt.Errorf("%w: limit must be 1-1000", ErrInvalid)
	case f.Limit == 0:
		f.Limit = 100
	}
	return f, nil
}

// Validate refuses a filter List would refuse (ErrInvalid).
func (f Filter) Validate() error {
	_, err := f.normalized()
	return err
}

// List reads decisions newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Decision, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	f, err := f.normalized()
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+decisionColumns+`
		FROM sage.shadow_decision
		WHERE ($1 = '' OR action_class = $1) AND ($2 = '' OR status = $2)
		  AND ($3 = '' OR score = $3)
		ORDER BY recorded_at DESC, id DESC LIMIT $4`, f.Class, f.Status, f.Score, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("shadow: list decisions: %w", err)
	}
	return collect(rows)
}

func collect(rows pgx.Rows) ([]Decision, error) {
	defer rows.Close()
	out := []Decision{}
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("shadow: scan decision: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shadow: read decisions: %w", err)
	}
	return out, nil
}

// classFamily is the trust family of a ledger class name.
func classFamily(class string) string {
	return string(earned.SelfFamilyFor(earned.ActionClass(class)))
}
