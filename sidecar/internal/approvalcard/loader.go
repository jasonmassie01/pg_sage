package approvalcard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// Loader reads cards of one monitored database.
type Loader struct {
	Pool       *pgxpool.Pool
	Database   string
	TrustLevel string
	Now        func() time.Time
}

func (l Loader) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Card returns the card of one queue item (any status).
func (l Loader) Card(ctx context.Context, queueID int) (Card, error) {
	if l.Pool == nil {
		return Card{}, errors.New("approvalcard: no database pool")
	}
	a, err := store.NewActionStore(l.Pool).GetByID(ctx, queueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Card{}, fmt.Errorf("%w: %d", ErrNotFound, queueID)
	}
	if err != nil {
		return Card{}, fmt.Errorf("approvalcard: read queue item %d: %w", queueID, err)
	}
	return l.ForAction(ctx, *a)
}

// Pending returns the cards of every pending queue item.
func (l Loader) Pending(ctx context.Context) ([]Card, error) {
	if l.Pool == nil {
		return nil, errors.New("approvalcard: no database pool")
	}
	actions, err := store.NewActionStore(l.Pool).ListPending(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("approvalcard: list pending: %w", err)
	}
	out := make([]Card, 0, len(actions))
	for _, a := range actions {
		c, err := l.ForAction(ctx, a)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ForAction reads a queue row's card inputs and assembles its card.
func (l Loader) ForAction(ctx context.Context, a store.QueuedAction) (Card, error) {
	in := Inputs{Database: l.Database, Action: a, TrustLevel: l.TrustLevel, Now: l.now()}
	if c, ok := executor.ContractForQueuedAction(a); ok {
		in.Contract = &c
	}
	var err error
	if in.Finding, err = l.finding(ctx, a.FindingID); err != nil {
		return Card{}, err
	}
	if in.Revision, err = l.revision(ctx, a); err != nil {
		return Card{}, err
	}
	if in.Decision, err = l.decision(ctx, a.ID); err != nil {
		return Card{}, err
	}
	if in.Rejection, err = l.rejection(ctx, a); err != nil {
		return Card{}, err
	}
	if in.Snooze, err = l.snooze(ctx, a.ID); err != nil {
		return Card{}, err
	}
	c := Assemble(in)
	if c.ShadowHistory, err = l.shadowHistory(ctx, a); err != nil {
		return Card{}, err
	}
	return c, nil
}

// optional maps "no row" to nil and wraps every other error.
func optional[T any](what string, v *T, err error) (*T, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("approvalcard: read %s: %w", what, err)
	}
	return v, nil
}

func decodeJSON(raw []byte, out any, what string) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("approvalcard: decode %s: %w", what, err)
	}
	return nil
}

func (l Loader) finding(ctx context.Context, id int) (*FindingRow, error) {
	var f FindingRow
	var raw []byte
	err := l.Pool.QueryRow(ctx, `/* pg_sage */ SELECT id, category, severity,
		COALESCE(object_type, ''), COALESCE(object_identifier, ''), COALESCE(title, ''),
		COALESCE(recommendation, ''), detail FROM sage.findings WHERE id = $1`, id).
		Scan(&f.ID, &f.Category, &f.Severity, &f.ObjectType, &f.Object, &f.Title,
			&f.Recommendation, &raw)
	got, err := optional("finding", &f, err)
	if got == nil || err != nil {
		return got, err
	}
	return got, decodeJSON(raw, &got.Detail, "finding detail")
}

func (l Loader) revision(ctx context.Context, a store.QueuedAction) (*RevisionRow, error) {
	if a.RecommendationID == nil || a.RecommendationRevision == nil {
		return nil, nil
	}
	r := RevisionRow{ID: *a.RecommendationID, Revision: *a.RecommendationRevision}
	var raw []byte
	err := l.Pool.QueryRow(ctx, `/* pg_sage */ SELECT content_hash, title,
		recommendation, evidence FROM sage.recommendation_revision
		WHERE recommendation_id = $1 AND revision = $2`, r.ID, r.Revision).
		Scan(&r.ContentHash, &r.Title, &r.Recommendation, &raw)
	got, err := optional("recommendation revision", &r, err)
	if got == nil || err != nil {
		return got, err
	}
	return got, decodeJSON(raw, &got.Evidence, "revision evidence")
}

func (l Loader) decision(ctx context.Context, queueID int) (*DecisionRow, error) {
	var d DecisionRow
	var raw []byte
	err := l.Pool.QueryRow(ctx, `/* pg_sage */ SELECT id, verdict, risk_tier, reason,
		guardrails, evidence_id, created_at FROM sage.decision WHERE queue_id = $1
		ORDER BY id DESC LIMIT 1`, queueID).Scan(&d.ID, &d.Verdict, &d.RiskTier,
		&d.Reason, &raw, &d.EvidenceID, &d.CreatedAt)
	got, err := optional("policy decision", &d, err)
	if got == nil || err != nil {
		return got, err
	}
	return got, decodeJSON(raw, &got.Guardrails, "decision guardrails")
}

func (l Loader) rejection(ctx context.Context, a store.QueuedAction) (*RejectionRow, error) {
	var r RejectionRow
	var decided *time.Time
	err := l.Pool.QueryRow(ctx, `/* pg_sage */ SELECT id, decided_at, COALESCE(reason, '')
		FROM sage.action_queue WHERE status = 'rejected' AND id <> $1
		  AND proposed_sql = $2
		ORDER BY decided_at DESC NULLS LAST, id DESC LIMIT 1`, a.ID, a.ProposedSQL).
		Scan(&r.QueueID, &decided, &r.Reason)
	if decided != nil {
		r.DecidedAt = *decided
	}
	return optional("earlier rejection", &r, err)
}

func (l Loader) snooze(ctx context.Context, queueID int) (*SnoozeRow, error) {
	var until *time.Time
	var s SnoozeRow
	err := l.Pool.QueryRow(ctx, `/* pg_sage */ SELECT snoozed_until,
		COALESCE(snoozed_by, 0), COALESCE(snooze_reason, '')
		FROM sage.action_queue WHERE id = $1`, queueID).Scan(&until, &s.By, &s.Reason)
	if err != nil || until == nil {
		return optional("snooze", (*SnoozeRow)(nil), err)
	}
	s.Until = *until
	return &s, nil
}
