package grants

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MaxPendingRequests bounds ListPendingRequests.
const MaxPendingRequests = 200

// ListPendingRequests lists every agent's unexpired pending capability
// requests in one database, oldest first: the inbox's read (ux-01a).
// limit outside 1..MaxPendingRequests is MaxPendingRequests.
func ListPendingRequests(ctx context.Context, q Querier, limit int) ([]Request, error) {
	if limit < 1 || limit > MaxPendingRequests {
		limit = MaxPendingRequests
	}
	rows, err := q.Query(ctx, `/* pg_sage guard_grant_request_pending v1 */
		SELECT `+requestColumns+` FROM sage.guard_grant_requests
		WHERE status = 'pending' AND expires_at > now()
		ORDER BY requested_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("grants: listing pending requests: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Request, error) {
		return scanRequest(r)
	})
	if err != nil {
		return nil, fmt.Errorf("grants: listing pending requests: %w", err)
	}
	if items == nil {
		items = []Request{}
	}
	return items, nil
}
