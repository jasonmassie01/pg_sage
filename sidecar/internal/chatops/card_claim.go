package chatops

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ClaimFence is the leader lease a follow-up claim must still hold: the
// lease row's scope, holder and epoch. The zero ClaimFence claims at epoch
// 0 without a lease check (leader election disabled).
type ClaimFence struct {
	Scope  string
	Holder string
	Epoch  int64
}

// claimFollowupSQL claims a card's follow-up for one leader epoch. A
// claim moves only to the same or a later epoch, or once it is 5 minutes
// old (a leader that died mid-post); a fenced claim also needs the lease
// to be held at that epoch now. Contenders serialize on the row lock and
// the later one re-checks against the winner's claim.
const claimFollowupSQL = `/* pg_sage */ UPDATE sage.approval_card_deliveries AS d
	SET followup_claim_epoch = $2, followup_claimed_at = now()
	WHERE d.id = $1 AND d.followed_up_at IS NULL
	  AND (d.followup_claim_epoch IS NULL OR d.followup_claim_epoch <= $2
	       OR d.followup_claimed_at < now() - interval '5 minutes')
	  AND ($4::text = '' OR EXISTS (SELECT 1 FROM sage.fleet_leader_lease AS l
	       WHERE l.scope = $3::text AND l.holder = $4 AND l.epoch = $2
	         AND l.expires_at > now()))
	RETURNING d.id`

// ClaimFollowup claims delivery id's follow-up under fence. claimed is
// false when the follow-up is done, another leader holds a newer or live
// claim, or fence's lease is no longer current: then nothing may be posted.
func (s *CardStore) ClaimFollowup(ctx context.Context, id int64,
	fence ClaimFence) (bool, error) {
	if fence.Holder == "" {
		fence.Scope, fence.Epoch = "", 0
	}
	var got int64
	err := s.pool.QueryRow(ctx, claimFollowupSQL, id, fence.Epoch, fence.Scope,
		fence.Holder).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chatops: claim approval follow-up %d: %w", id, err)
	}
	return true, nil
}
