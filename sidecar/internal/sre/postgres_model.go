package sre

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// maxModelEventBytes bounds a model event payload as encoded here; the
// jsonb text form (checked at 16 KiB by the schema) is slightly larger.
const maxModelEventBytes = 12 << 10

var modelEventTypes = map[string]bool{EventModelReviewed: true,
	EventModelRejected: true, EventModelDisagreed: true}

// RecordEvent appends one model-turn event (model_reviewed,
// model_rejected or model_disagreed) to the investigation's hash chain
// under the lease. A stale or expired lease records nothing
// (ErrLeaseLost).
func (s *PostgresStore) RecordEvent(ctx context.Context, lease Lease, typ string,
	payload map[string]any) error {
	if err := lease.validate(); err != nil {
		return err
	}
	if !modelEventTypes[typ] {
		return fmt.Errorf("%w: event type %q is not a model event", ErrInvalidRequest, typ)
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > maxModelEventBytes {
		return fmt.Errorf("%w: model event payload too large or unencodable",
			ErrInvalidRequest)
	}
	return s.inTx(ctx, "record event", func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM sage.sre_investigations
			WHERE `+leaseGuard+` FOR UPDATE`, leaseArgs(lease)...).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, lease.Scope, lease.InvestigationID, typ,
			workerActor(lease), payload)
	})
}

// Limits are the per-investigation ceilings and daily model allocation
// this store enforces.
func (s *PostgresStore) Limits() Limits { return s.limits }
