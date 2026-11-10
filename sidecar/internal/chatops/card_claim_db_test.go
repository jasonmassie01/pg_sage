package chatops

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// issuedDelivery issues a card and returns its delivery id.
func issuedDelivery(t *testing.T, s *CardStore, queueID int) int64 {
	t.Helper()
	ctx := context.Background()
	token, err := s.Issue(ctx, cardIssue(queueID))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	d, err := s.Lookup(ctx, token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	return d.ID
}

// leaseAt sets scope's lease to holder at epoch, live for a minute.
func leaseAt(t *testing.T, pool *pgxpool.Pool, scope, holder string, epoch int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.fleet_leader_lease
		(scope, holder, epoch, expires_at) VALUES ($1, $2, $3, now() + interval '1 minute')
		ON CONFLICT (scope) DO UPDATE SET holder = EXCLUDED.holder,
		epoch = EXCLUDED.epoch, expires_at = EXCLUDED.expires_at`,
		scope, holder, epoch); err != nil {
		t.Fatalf("lease: %v", err)
	}
}

func claimRow(t *testing.T, pool *pgxpool.Pool, id int64) *int64 {
	t.Helper()
	var epoch *int64
	if err := pool.QueryRow(context.Background(), `SELECT followup_claim_epoch
		FROM sage.approval_card_deliveries WHERE id = $1`, id).Scan(&epoch); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	return epoch
}

func mustClaim(t *testing.T, s *CardStore, id int64, f ClaimFence, want bool) {
	t.Helper()
	got, err := s.ClaimFollowup(context.Background(), id, f)
	if err != nil || got != want {
		t.Fatalf("claim %d under %+v = %v err=%v, want %v", id, f, got, err, want)
	}
}

func TestClaimFollowup_UnfencedClaimsUntilFollowedUp(t *testing.T) {
	s, pool, ctx := liveCards(t)
	id := issuedDelivery(t, s, 801)
	mustClaim(t, s, id, ClaimFence{}, true)
	if e := claimRow(t, pool, id); e == nil || *e != 0 {
		t.Fatalf("claim epoch = %v, want 0", e)
	}
	mustClaim(t, s, id, ClaimFence{}, true) // same epoch: a retry after a failed post
	if err := s.MarkFollowedUp(ctx, id, "verified"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	mustClaim(t, s, id, ClaimFence{}, false)
	mustClaim(t, s, 1<<62, ClaimFence{}, false) // unknown delivery
}

func TestClaimFollowup_FencedNeedsTheCurrentLease(t *testing.T) {
	s, pool, _ := liveCards(t)
	scope := "claim-" + t.Name()
	leaseAt(t, pool, scope, "B", 5)
	id := issuedDelivery(t, s, 802)
	mustClaim(t, s, id, ClaimFence{Scope: scope, Holder: "A", Epoch: 4}, false)
	mustClaim(t, s, id, ClaimFence{Scope: scope, Holder: "A", Epoch: 5}, false)
	mustClaim(t, s, id, ClaimFence{Scope: "", Holder: "B", Epoch: 5}, false)
	if e := claimRow(t, pool, id); e != nil {
		t.Fatalf("claim epoch = %v after refused claims, want none", *e)
	}
	mustClaim(t, s, id, ClaimFence{Scope: scope, Holder: "B", Epoch: 5}, true)
	if e := claimRow(t, pool, id); e == nil || *e != 5 {
		t.Fatalf("claim epoch = %v, want 5", e)
	}
}

func TestClaimFollowup_NewerEpochTakesOverOlderOnlyAfterExpiry(t *testing.T) {
	s, pool, ctx := liveCards(t)
	scope := "claim-" + t.Name()
	id := issuedDelivery(t, s, 803)
	leaseAt(t, pool, scope, "A", 4)
	mustClaim(t, s, id, ClaimFence{Scope: scope, Holder: "A", Epoch: 4}, true)
	leaseAt(t, pool, scope, "B", 5) // handover mid-cycle
	mustClaim(t, s, id, ClaimFence{Scope: scope, Holder: "B", Epoch: 5}, true)
	// The lease goes back to an older epoch number (rows reset): a live
	// newer claim is kept for 5 minutes, then taken over.
	leaseAt(t, pool, scope, "C", 3)
	mustClaim(t, s, id, ClaimFence{Scope: scope, Holder: "C", Epoch: 3}, false)
	if _, err := pool.Exec(ctx, `UPDATE sage.approval_card_deliveries
		SET followup_claimed_at = now() - interval '301 seconds' WHERE id = $1`, id); err != nil {
		t.Fatalf("age claim: %v", err)
	}
	mustClaim(t, s, id, ClaimFence{Scope: scope, Holder: "C", Epoch: 3}, true)
}

func TestClaimFollowup_RaceAtEpochs4And5OnlyFiveWins(t *testing.T) {
	s, pool, _ := liveCards(t)
	scope := "claim-" + t.Name()
	leaseAt(t, pool, scope, "B", 5)
	for i := 0; i < 20; i++ {
		id := issuedDelivery(t, s, 900+i)
		var wg sync.WaitGroup
		won := make([]bool, 2)
		errs := make([]error, 2)
		for j, f := range []ClaimFence{{Scope: scope, Holder: "A", Epoch: 4},
			{Scope: scope, Holder: "B", Epoch: 5}} {
			wg.Add(1)
			go func(j int, f ClaimFence) {
				defer wg.Done()
				won[j], errs[j] = s.ClaimFollowup(context.Background(), id, f)
			}(j, f)
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || won[0] || !won[1] {
			t.Fatalf("round %d: epoch 4 won=%v err=%v, epoch 5 won=%v err=%v; want "+
				"only 5", i, won[0], errs[0], won[1], errs[1])
		}
	}
}

func TestClaimFollowup_ErrorNamesTheDelivery(t *testing.T) {
	s, _, ctx := liveCards(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ClaimFollowup(cctx, 4242, ClaimFence{}); err == nil ||
		!strings.Contains(err.Error(), "claim approval follow-up 4242") {
		t.Fatalf("err = %v, want one naming delivery 4242", err)
	}
}
