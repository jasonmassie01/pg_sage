package earned

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// recordRootAuthority appends the model-root history entry family's
// current grant needs (event builds it), if any, and reports its type.
// One transaction under an advisory lock per deployment, database and
// family, so concurrent askers (replicas, the reconcile loop and an
// investigation) record a change once.
func (s *PostgresStore) recordRootAuthority(ctx context.Context, family Family, granted bool,
	event func(EventType) Event) (EventType, bool, error) {
	var typ EventType
	var recorded bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"sage.model_root:"+s.deployment+":"+s.database+":"+string(family)); err != nil {
			return err
		}
		var last *Event
		var t string
		err := tx.QueryRow(ctx, `SELECT event_type FROM sage.sre_autonomy_events
			WHERE deployment_id = $1 AND database_name = $2 AND family = $3
			  AND action_class = $4 AND event_type IN ($5, $6)
			ORDER BY id DESC LIMIT 1`, s.deployment, s.database, string(family),
			string(ModelRootClass), string(EventRootAuthorityGranted),
			string(EventRootAuthorityRevoked)).Scan(&t)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			last = &Event{Type: EventType(t)}
		}
		next, changed := rootAuthorityTransition(last, granted)
		if !changed {
			return nil
		}
		if err := s.appendEvent(ctx, tx, event(next)); err != nil {
			return err
		}
		typ, recorded = next, true
		return nil
	})
	if err != nil {
		return "", false, storeErr("record model-root authority", err)
	}
	return typ, recorded, nil
}
