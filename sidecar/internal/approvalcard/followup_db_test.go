package approvalcard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// A snoozed card is answered by a new card; the verdict is posted once per
// chat, to the newest card, and a card that is not final yet waits its
// turn instead of holding the head of the queue.

// issuingDispatch delivers a card to each channel the way notify does:
// one token per card per channel, recorded in the control database.
type issuingDispatch struct {
	mu       sync.Mutex
	iss      TokenIssuer
	channels []int
	tokens   map[int][]string
}

func (d *issuingDispatch) Dispatch(ctx context.Context, evt notify.Event) error {
	ref, ok := notify.ApprovalCardOf(evt)
	if !ok {
		return errors.New("event carries no approval card")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tokens == nil {
		d.tokens = map[int][]string{}
	}
	for _, ch := range d.channels {
		token, err := d.iss.Issue(ctx, notify.Channel{ID: ch}, ref)
		if err != nil {
			return err
		}
		d.tokens[ch] = append(d.tokens[ch], token)
	}
	return nil
}

func (d *issuingDispatch) latest(t *testing.T, ch int) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.tokens[ch]) == 0 {
		t.Fatalf("no card sent to channel %d", ch)
	}
	return d.tokens[ch][len(d.tokens[ch])-1]
}

func ordersPool(pool *pgxpool.Pool) func(string) *pgxpool.Pool {
	return func(db string) *pgxpool.Pool {
		if db == "orders" {
			return pool
		}
		return nil
	}
}

func clearDeliveries(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.approval_card_deliveries`); err != nil {
		t.Fatal(err)
	}
}

// markExecuted records that the queue item ran with outcome.
func markExecuted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queueID,
	findingID int, outcome string) {
	t.Helper()
	var logID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, outcome) VALUES ('create_index', $1, 'CREATE INDEX CONCURRENTLY x',
		$2) RETURNING id`, findingID, outcome).Scan(&logID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET status = 'executed',
		action_log_id = $2, decided_by = 3, decided_at = now(), snoozed_until = NULL
		WHERE id = $1`, queueID, logID); err != nil {
		t.Fatal(err)
	}
}

// snoozeAndResend snoozes the item from channel 21's newest card, ends the
// snooze and runs the re-send.
func snoozeAndResend(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	queueID, round int, d *issuingDispatch, n *Notifier, l Loader) {
	t.Helper()
	cards := d.iss.Store
	if _, err := cards.Consume(ctx, d.latest(t, 21), 3, chatops.DecisionSnooze,
		int64(100+round)); err != nil {
		t.Fatalf("round %d: snooze card: %v", round, err)
	}
	err := store.NewActionStore(pool).Snooze(ctx, queueID, 3, time.Now().Add(time.Hour), "later")
	if err != nil {
		t.Fatalf("round %d: snooze: %v", round, err)
	}
	endSnooze(t, ctx, pool, queueID)
	if sent, err := RenotifySnoozed(ctx, l, n); err != nil || sent != 1 {
		t.Fatalf("round %d: re-send = %d, %v", round, sent, err)
	}
}

func TestTwiceSnoozedActionGetsOneFollowupPerChannel(t *testing.T) {
	pool, ctx := livePool(t)
	clearSnoozes(t, ctx, pool)
	clearDeliveries(t, ctx, pool)
	cards := chatops.NewCardStore(pool)
	queueID, findingID := queueOptimizerIndex(t, ctx, pool)
	l := Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"}
	d := &issuingDispatch{iss: TokenIssuer{Store: cards}, channels: []int{21, 22}}
	n := NewNotifier(d, l)
	if err := n.Dispatch(ctx, notify.QueuedApprovalEvent("Index recommendation",
		"CREATE INDEX ...", "orders", "moderate", queueID)); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	for round := 0; round < 2; round++ {
		snoozeAndResend(t, ctx, pool, queueID, round, d, n, l)
	}
	requireOneOpenCardPerChannel(t, ctx, pool, queueID)
	if _, err := cards.Consume(ctx, d.latest(t, 21), 3, chatops.DecisionApprove,
		777); err != nil {
		t.Fatalf("approve: %v", err)
	}
	markExecuted(t, ctx, pool, queueID, findingID, "success")
	chans := &fakeChannels{}
	f := &Followups{Store: cards, Sender: chans, Pools: ordersPool(pool)}
	if sent, err := f.RunOnce(ctx); sent != 2 || err != nil {
		t.Fatalf("follow-ups = %d, %v; want one per channel", sent, err)
	}
	perChannel := map[int][]notify.Event{}
	for _, s := range chans.sent {
		perChannel[s.channel] = append(perChannel[s.channel], s.evt)
	}
	if len(perChannel[21]) != 1 || len(perChannel[22]) != 1 || len(chans.sent) != 2 {
		t.Fatalf("follow-ups per channel: %d to 21, %d to 22, %d in all",
			len(perChannel[21]), len(perChannel[22]), len(chans.sent))
	}
	if got := perChannel[21][0].Data[notify.DataReplyTo]; got != int64(777) {
		t.Fatalf("channel 21 follow-up replies to %v, want the approval message 777", got)
	}
	if _, ok := perChannel[22][0].Data[notify.DataReplyTo]; ok {
		t.Fatal("channel 22 decided nothing, yet its follow-up is a reply")
	}
	if sent, err := f.RunOnce(ctx); sent != 0 || err != nil || len(chans.sent) != 2 {
		t.Fatalf("second run = %d, %v, %d posts in all", sent, err, len(chans.sent))
	}
}

// requireOneOpenCardPerChannel checks that each of the two channels holds
// one open card and the two it replaced, closed as superseded.
func requireOneOpenCardPerChannel(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	queueID int) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT channel_id,
		count(*) FILTER (WHERE followed_up_at IS NULL),
		count(*) FILTER (WHERE followup_verdict = 'superseded')
		FROM sage.approval_card_deliveries WHERE queue_id = $1
		GROUP BY channel_id ORDER BY channel_id`, queueID)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for rows.Next() {
		var ch, open, superseded int
		if err := rows.Scan(&ch, &open, &superseded); err != nil {
			t.Fatal(err)
		}
		seen++
		if open != 1 || superseded != 2 {
			t.Errorf("channel %d: %d open, %d superseded; want 1 and 2", ch, open, superseded)
		}
	}
	rows.Close()
	if seen != 2 {
		t.Fatalf("cards went to %d channels, want 2", seen)
	}
}

// recordingStore records the card ids each run lists.
type recordingStore struct {
	*chatops.CardStore
	runs [][]int64
}

func (r *recordingStore) PendingFollowups(ctx context.Context,
	limit int) ([]chatops.CardDelivery, error) {
	out, err := r.CardStore.PendingFollowups(ctx, limit)
	ids := make([]int64, 0, len(out))
	for _, d := range out {
		ids = append(ids, d.ID)
	}
	r.runs = append(r.runs, ids)
	return out, err
}

func TestFollowupsReachNewerCardsBehindAFullBatch(t *testing.T) {
	pool, ctx := livePool(t)
	clearDeliveries(t, ctx, pool)
	insertWaitingCards(t, ctx, pool, 501)
	verified := executedWithOutcome(t, ctx, pool, "success", "")
	cards := chatops.NewCardStore(pool)
	if _, err := cards.Issue(ctx, chatops.CardIssue{ChannelID: 900, Database: "orders",
		QueueID: verified, CardHash: "h", Title: "Index", ExpiresAt: time.Now().Add(
			time.Hour)}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingStore{CardStore: cards}
	chans := &fakeChannels{}
	f := &Followups{Store: rec, Sender: chans, Pools: ordersPool(pool)}
	if sent, err := f.RunOnce(ctx); sent != 0 || err != nil || len(rec.runs[0]) != 500 {
		t.Fatalf("first run = %d, %v, %d listed", sent, err, len(rec.runs[0]))
	}
	// Deferred by a eighth of their age: two hours old, back in 15 minutes.
	var deferred int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.approval_card_deliveries
		WHERE id = ANY($1) AND followed_up_at IS NULL
		  AND next_check_at BETWEEN now() + interval '14 minutes'
		                        AND now() + interval '16 minutes'`,
		rec.runs[0]).Scan(&deferred); err != nil {
		t.Fatal(err)
	}
	if deferred != 500 {
		t.Fatalf("%d of the 500 listed cards were deferred by 15 minutes", deferred)
	}
	sent, err := f.RunOnce(ctx)
	if sent != 1 || err != nil || len(chans.sent) != 1 || chans.sent[0].channel != 900 {
		t.Fatalf("second run = %d, %v, posts %+v", sent, err, chans.sent)
	}
	first := map[int64]bool{}
	for _, id := range rec.runs[0] {
		first[id] = true
	}
	if len(rec.runs[1]) != 2 {
		t.Fatalf("second run listed %d cards, want the 501st and the final one",
			len(rec.runs[1]))
	}
	for _, id := range rec.runs[1] {
		if first[id] {
			t.Fatalf("second run re-read deferred card %d", id)
		}
	}
}

// insertWaitingCards adds n two-hour-old cards, one per channel, of an
// item that is still pending.
func insertWaitingCards(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	waiting, _ := queueOptimizerIndex(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.approval_card_deliveries
		(token_sha256, channel_id, database_name, queue_id, card_hash, expires_at,
		 created_at)
		SELECT sha256(convert_to('bound-' || g || '-' || clock_timestamp(), 'UTF8')), g,
		       'orders', $1, 'h', now() + interval '1 day', now() - interval '2 hours'
		  FROM generate_series(1, $2::int) g`, waiting, n); err != nil {
		t.Fatal(err)
	}
}

func TestFollowupDelay(t *testing.T) {
	cases := []struct{ age, want time.Duration }{
		{-time.Hour, time.Minute}, // clock skew: the floor
		{0, time.Minute},
		{8 * time.Minute, time.Minute},
		{8*time.Minute + 8*time.Second, time.Minute + time.Second},
		{16 * time.Minute, 2 * time.Minute},
		{2 * time.Hour, 15 * time.Minute},
		{8 * time.Hour, time.Hour},
		{14 * 24 * time.Hour, time.Hour},
	}
	for _, c := range cases {
		if got := followupDelay(c.age); got != c.want {
			t.Errorf("followupDelay(%v) = %v, want %v", c.age, got, c.want)
		}
	}
}

// A chat that decides from the card sent before the re-send still gets
// one follow-up, as a reply to that decision.
func TestFollowupRepliesToAnOlderCardPressedAfterAResend(t *testing.T) {
	pool, ctx := livePool(t)
	clearSnoozes(t, ctx, pool)
	clearDeliveries(t, ctx, pool)
	cards := chatops.NewCardStore(pool)
	queueID, findingID := queueOptimizerIndex(t, ctx, pool)
	l := Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"}
	d := &issuingDispatch{iss: TokenIssuer{Store: cards}, channels: []int{21, 22}}
	n := NewNotifier(d, l)
	if err := n.Dispatch(ctx, notify.QueuedApprovalEvent("Index recommendation",
		"CREATE INDEX ...", "orders", "moderate", queueID)); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	first22 := d.latest(t, 22)
	snoozeAndResend(t, ctx, pool, queueID, 0, d, n, l)
	if _, err := cards.Consume(ctx, first22, 3, chatops.DecisionApprove, 888); err != nil {
		t.Fatalf("approve from channel 22's first card: %v", err)
	}
	markExecuted(t, ctx, pool, queueID, findingID, "success")
	chans := &fakeChannels{}
	f := &Followups{Store: cards, Sender: chans, Pools: ordersPool(pool)}
	if sent, err := f.RunOnce(ctx); sent != 2 || err != nil {
		t.Fatalf("follow-ups = %d, %v; want one per channel", sent, err)
	}
	var to22 []notify.Event
	for _, s := range chans.sent {
		if s.channel == 22 {
			to22 = append(to22, s.evt)
		}
	}
	if len(to22) != 1 || to22[0].Data[notify.DataReplyTo] != int64(888) {
		t.Fatalf("channel 22 follow-ups = %+v, want one reply to message 888", to22)
	}
}
