package approvalcard

// fleet-03: each follow-up is claimed under the leader lease before it is
// posted. ordersPool is shared with followup_db_test.go.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
)

func TestFollowupsClaimFailureSkipsThePost(t *testing.T) {
	pool, ctx := livePool(t)
	verified := executedWithOutcome(t, ctx, pool, "success", "")
	st := &claimStore{err: errors.New("control db lost")}
	st.items = []chatops.CardDelivery{{ID: 77, ChannelID: 11, Database: "orders",
		QueueID: verified, CreatedAt: time.Now()}}
	chans := &fakeChannels{}
	f := &Followups{Store: st, Sender: chans, Pools: ordersPool(pool)}
	n, err := f.RunOnce(ctx)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "claim follow-up 77") {
		t.Fatalf("run = %d, %v; want an error naming delivery 77", n, err)
	}
	if len(chans.sent) != 0 || len(st.marked) != 0 {
		t.Fatalf("sent %d, marked %v; want neither after a failed claim", len(chans.sent),
			st.marked)
	}
	st.err, st.claimed = nil, false // another leader holds it
	if n, err := f.RunOnce(ctx); n != 0 || err != nil || len(chans.sent) != 0 {
		t.Fatalf("unclaimed run = %d, %v, sent %d; want a silent skip", n, err,
			len(chans.sent))
	}
}

// taggedChannels counts posts per follower.
type taggedChannels struct {
	mu    *sync.Mutex
	posts map[string]int
	tag   string
}

func (c taggedChannels) SendToChannel(context.Context, int, notify.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts[c.tag]++
	return nil
}

func TestFollowupsRaceAtEpochs4And5PostsOnceByFive(t *testing.T) {
	pool, ctx := livePool(t)
	scope := "followup-race-" + time.Now().Format("150405.000000000")
	if _, err := pool.Exec(ctx, `INSERT INTO sage.fleet_leader_lease
		(scope, holder, epoch, expires_at) VALUES ($1, 'B', 5, now() + interval '1 minute')`,
		scope); err != nil {
		t.Fatalf("lease: %v", err)
	}
	cards := chatops.NewCardStore(pool)
	verified := executedWithOutcome(t, ctx, pool, "success", "")
	if _, err := cards.Issue(ctx, chatops.CardIssue{ChannelID: 11, Database: "orders",
		QueueID: verified, CardHash: "h-" + scope, Title: "Index",
		ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("issue: %v", err)
	}
	mu, posts := &sync.Mutex{}, map[string]int{}
	follower := func(holder string, epoch int64) *Followups {
		return &Followups{Store: cards, Pools: ordersPool(pool),
			Sender: taggedChannels{mu: mu, posts: posts, tag: holder},
			Fence: func() (chatops.ClaimFence, bool) {
				return chatops.ClaimFence{Scope: scope, Holder: holder, Epoch: epoch}, true
			}}
	}
	var wg sync.WaitGroup
	for _, f := range []*Followups{follower("A", 4), follower("B", 5)} {
		wg.Add(1)
		go func(f *Followups) {
			defer wg.Done()
			_, _ = f.RunOnce(ctx)
		}(f)
	}
	wg.Wait()
	if posts["A"] != 0 || posts["B"] < 1 {
		t.Fatalf("posts = %v, want none by epoch 4 and B's post", posts)
	}
	var claimed int64
	var done bool
	if err := pool.QueryRow(ctx, `SELECT followup_claim_epoch, followed_up_at IS NOT NULL
		FROM sage.approval_card_deliveries WHERE card_hash = $1`, "h-"+scope).
		Scan(&claimed, &done); err != nil || claimed != 5 || !done {
		t.Fatalf("delivery claim=%d done=%v err=%v, want claimed at 5 and closed",
			claimed, done, err)
	}
	before := posts["B"]
	if _, err := follower("B", 5).RunOnce(ctx); err != nil || posts["B"] != before {
		t.Fatalf("second run posted again: %v, %v", posts, err)
	}
}
