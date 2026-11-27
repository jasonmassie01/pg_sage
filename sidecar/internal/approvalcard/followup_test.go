package approvalcard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// No concurrency test here: the claim race runs on PostgreSQL in
// followup_db_test.go and chatops/card_claim_db_test.go.

// claimStore records claims and answers them from claimed/err.
type claimStore struct {
	fakeDeliveries
	fences  []chatops.ClaimFence
	claimed bool
	err     error
}

func (c *claimStore) ClaimFollowup(_ context.Context, _ int64,
	f chatops.ClaimFence) (bool, error) {
	c.fences = append(c.fences, f)
	return c.claimed, c.err
}

var delivery = chatops.CardDelivery{ID: 42, QueueID: 7, ChannelID: 3}

func TestFollowupClaim_NotLeaderSkipsWithoutAClaim(t *testing.T) {
	st := &claimStore{claimed: true}
	f := &Followups{Store: st, Fence: func() (chatops.ClaimFence, bool) {
		return chatops.ClaimFence{}, false
	}}
	ok, err := f.claim(context.Background(), delivery)
	if ok || err != nil || len(st.fences) != 0 {
		t.Fatalf("claim = %v err=%v claims=%d, want a skip with no claim", ok, err,
			len(st.fences))
	}
}

func TestFollowupClaim_PassesTheLeaderFence(t *testing.T) {
	want := chatops.ClaimFence{Scope: "fleet:x", Holder: "B", Epoch: 5}
	st := &claimStore{claimed: true}
	f := &Followups{Store: st, Fence: func() (chatops.ClaimFence, bool) { return want, true }}
	ok, err := f.claim(context.Background(), delivery)
	if !ok || err != nil || len(st.fences) != 1 || st.fences[0] != want {
		t.Fatalf("claim = %v err=%v fences=%+v, want one claim under %+v", ok, err,
			st.fences, want)
	}
	st.claimed = false
	if ok, err := f.claim(context.Background(), delivery); ok || err != nil {
		t.Fatalf("lost claim = %v err=%v, want false and no error", ok, err)
	}
}

func TestFollowupClaim_NilFenceClaimsUnfenced(t *testing.T) {
	st := &claimStore{claimed: true}
	f := &Followups{Store: st}
	if ok, err := f.claim(context.Background(), delivery); !ok || err != nil ||
		len(st.fences) != 1 || st.fences[0] != (chatops.ClaimFence{}) {
		t.Fatalf("claim = %v err=%v fences=%+v, want one zero-fence claim", ok, err,
			st.fences)
	}
}

func TestFollowupClaim_ErrorNamesTheDelivery(t *testing.T) {
	st := &claimStore{err: errors.New("connection reset")}
	f := &Followups{Store: st}
	ok, err := f.claim(context.Background(), delivery)
	if ok || err == nil || !strings.Contains(err.Error(), "claim follow-up 42") ||
		!strings.Contains(err.Error(), "queue item 7") ||
		!errors.Is(err, st.err) {
		t.Fatalf("claim = %v err=%v, want an error naming delivery 42 and its cause", ok, err)
	}
}
