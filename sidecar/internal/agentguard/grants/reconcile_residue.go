package grants

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// incompleteSQL uses guard_grants_incomplete_idx.
const incompleteSQL = `/* pg_sage guard_grant_reconcile v1 */
SELECT id, principal_id, object_oid::int8 FROM sage.guard_grants
WHERE state = 'revoke_incomplete' ORDER BY id LIMIT $1`

// recheckIncomplete completes revoke_incomplete grants whose foreign
// residue the other grantor has since removed: D10 stops denying the
// object then.
func (m *Manager) recheckIncomplete(ctx context.Context, t Target, f Fence, limit int,
	rep *ExpiryReport) error {
	rows, err := t.Pool.Query(ctx, incompleteSQL, limit)
	if err != nil {
		return fmt.Errorf("grants: reading incomplete revokes: %w", err)
	}
	type item struct {
		id  int64
		pid string
		oid int64
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		return it, r.Scan(&it.id, &it.pid, &it.oid)
	})
	if err != nil {
		return fmt.Errorf("grants: reading incomplete revokes: %w", err)
	}
	for _, it := range items {
		residue, err := objectResidue(ctx, t.Pool, agentguard.BrokerRoleName(it.pid),
			uint32(it.oid))
		if err != nil {
			rep.Failed[it.id] = err
			continue
		}
		if len(residue) > 0 {
			continue
		}
		if err := completeRevoke(ctx, t.Pool, f, it.id); err != nil {
			return err
		}
		rep.Completed = append(rep.Completed, it.id)
	}
	return nil
}

func completeRevoke(ctx context.Context, q Querier, f Fence, id int64) error {
	release, err := holdFence(ctx, f)
	if err != nil {
		return err
	}
	defer release()
	_, err = q.Exec(ctx, `/* pg_sage guard_grant_reconcile v1 */
		UPDATE sage.guard_grants SET state = 'revoked',
		  revoke_detail = left(COALESCE(revoke_detail, '') || '; residue cleared', 4000)
		WHERE id = $1 AND state = 'revoke_incomplete'`, id)
	if err != nil {
		return fmt.Errorf("grants: completing revoke of %d: %w", id, err)
	}
	return nil
}
