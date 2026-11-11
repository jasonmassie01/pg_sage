package recommendation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier runs a read; *pgxpool.Pool, *pgx.Conn and pgx.Tx satisfy it.
type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// FindingHead is the newest recommendation of one finding, with when it
// last entered applied (nil when it never was).
type FindingHead struct {
	Recommendation
	AppliedAt *time.Time
}

// latestByFindingSQL is the newest head per finding, in one statement
// over the recommendation_finding index; applied_at comes from the
// head's own history (recommendation_transition_history).
const latestByFindingSQL = `/* pg_sage */ SELECT DISTINCT ON (r.finding_id) ` +
	headColumns + `,
	(SELECT max(t.created_at) FROM sage.recommendation_transition t
	  WHERE t.recommendation_id = r.id AND t.to_state = 'applied')
	FROM sage.recommendation r
	WHERE r.finding_id = ANY($1::bigint[])
	ORDER BY r.finding_id, r.id DESC`

// LatestByFindingIDs returns the newest recommendation head of each
// finding in ids, keyed by finding id, in one query (cases-10). Findings
// without a head are absent from the map. The heads' state machine is
// read, never changed.
func LatestByFindingIDs(
	ctx context.Context, q Querier, ids []int64,
) (map[int64]FindingHead, error) {
	out := make(map[int64]FindingHead, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if q == nil {
		return nil, fmt.Errorf("read recommendations of %d findings: no database", len(ids))
	}
	rows, err := q.Query(ctx, latestByFindingSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("read recommendations of %d findings: %w", len(ids), err)
	}
	heads, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (FindingHead, error) {
		var h FindingHead
		var state string
		dest := append(headDest(&h.Recommendation, &state), &h.AppliedAt)
		err := row.Scan(dest...)
		h.State = State(state)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan recommendations of %d findings: %w", len(ids), err)
	}
	for _, h := range heads {
		if h.FindingID != nil {
			out[*h.FindingID] = h
		}
	}
	return out, nil
}
