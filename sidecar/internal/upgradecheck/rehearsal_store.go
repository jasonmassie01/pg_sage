package upgradecheck

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StoreRehearsal stores the rehearsal r on brief b (drill-02b): b is
// stored as StoreBrief stores it, and the latest row of its database and
// target then carries r, which later briefs carry forward. A rehearsal of
// another database or target major is refused.
func StoreRehearsal(ctx context.Context, pool *pgxpool.Pool, b Brief,
	r RehearsalReport) (StoredBrief, error) {
	if r.Database != b.Database || r.TargetMajor != b.Target.Major {
		return StoredBrief{}, fmt.Errorf("%w: a rehearsal of %s to %d cannot be stored on "+
			"the brief of %s to %d", ErrInvalidRehearsal, r.Database, r.TargetMajor,
			b.Database, b.Target.Major)
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return StoredBrief{}, fmt.Errorf("store the rehearsal: encode: %w", err)
	}
	b.Rehearsal = &r
	return storeBrief(ctx, pool, b, func(tx pgx.Tx, got *StoredBrief) error {
		if _, err := tx.Exec(ctx, storeTag+`UPDATE sage.upgrade_briefs
			SET brief = pg_catalog.jsonb_set(brief, '{rehearsal}', $2::jsonb)
			WHERE id = $1`, got.ID, doc); err != nil {
			return fmt.Errorf("attach the rehearsal: %w", err)
		}
		got.Rehearsal = &r
		return nil
	})
}
