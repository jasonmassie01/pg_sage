package agentguard

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Open freezes (agentgov-07): what GET /api/v1/agents/freezes shows, so
// the Agents page can show a kill's fleet or database flag and lift it.
// A read of the control database only, bounded to MaxListedFreezes rows.

// MaxListedFreezes bounds one freezes listing.
const MaxListedFreezes = 200

// ListedFreeze is one open sage.guard_freezes row. AfterKill marks a
// kill's freeze, whose lifting needs two admins.
type ListedFreeze struct {
	ID             int64           `json:"id"`
	Scope          string          `json:"scope"`
	Target         string          `json:"target"`
	Reason         string          `json:"reason"`
	SetBy          string          `json:"set_by"`
	SetAt          time.Time       `json:"set_at"`
	AfterKill      bool            `json:"after_kill"`
	PendingRelease *PendingRelease `json:"pending_release"`
}

// PendingRelease is the first admin's live request to lift a freeze; an
// expired request is no request, as the second admin's call treats it.
type PendingRelease struct {
	RequestID   int64     `json:"request_id"`
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// FreezeList is the open freezes, fleet first; Truncated says more were
// open than MaxListedFreezes.
type FreezeList struct {
	Items     []ListedFreeze `json:"freezes"`
	Truncated bool           `json:"truncated"`
}

type freezeQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// OpenFreezes lists the open freezes. Without the control database it
// fails closed with ErrUnavailable.
func (s *Switch) OpenFreezes(ctx context.Context) (FreezeList, error) {
	pool := s.store.Pool()
	if pool == nil {
		return FreezeList{Items: []ListedFreeze{}}, ErrUnavailable
	}
	return listFreezes(ctx, pool)
}

func listFreezes(ctx context.Context, q freezeQuerier) (FreezeList, error) {
	rows, err := q.Query(ctx, `/* pg_sage guard_freezes_list v1 */
		SELECT f.id, f.scope, f.target, f.reason, f.set_by, f.set_at,
		       f.kill_id IS NOT NULL, r.id, r.requested_by, r.requested_at, r.expires_at
		FROM sage.guard_freezes f
		LEFT JOIN sage.guard_unfreeze_requests r
		       ON r.freeze_id = f.id AND r.status = 'pending' AND r.expires_at > now()
		WHERE f.cleared_at IS NULL
		ORDER BY CASE f.scope WHEN 'fleet' THEN 0 WHEN 'database' THEN 1 ELSE 2 END,
		         f.set_at, f.id
		LIMIT $1`, MaxListedFreezes+1)
	if err != nil {
		return FreezeList{}, fmt.Errorf("agentguard: listing freezes: %w", err)
	}
	items, err := pgx.CollectRows(rows, scanListedFreeze)
	if err != nil {
		return FreezeList{}, fmt.Errorf("agentguard: listing freezes: %w", err)
	}
	list := FreezeList{Items: items}
	if len(items) > MaxListedFreezes {
		list.Items, list.Truncated = items[:MaxListedFreezes], true
	}
	return list, nil
}

func scanListedFreeze(row pgx.CollectableRow) (ListedFreeze, error) {
	var f ListedFreeze
	var reqID *int64
	var reqBy *string
	var reqAt, expires *time.Time
	if err := row.Scan(&f.ID, &f.Scope, &f.Target, &f.Reason, &f.SetBy, &f.SetAt,
		&f.AfterKill, &reqID, &reqBy, &reqAt, &expires); err != nil {
		return f, err
	}
	if reqID != nil && reqBy != nil && reqAt != nil && expires != nil {
		f.PendingRelease = &PendingRelease{RequestID: *reqID, RequestedBy: *reqBy,
			RequestedAt: *reqAt, ExpiresAt: *expires}
	}
	return f, nil
}
