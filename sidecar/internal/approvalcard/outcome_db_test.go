package approvalcard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
)

// After an approved card runs and pg_sage verifies it, the verdict is
// posted to every chat the card went to (as a reply when the decision was
// made there), with the numbers the verification recorded.

func executedWithOutcome(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	outcome, reason string) int {
	t.Helper()
	queueID, findingID := queueOptimizerIndex(t, ctx, pool)
	var logID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, outcome, rollback_reason) VALUES ('create_index', $1,
		'CREATE INDEX CONCURRENTLY x', $2, NULLIF($3, '')) RETURNING id`,
		findingID, outcome, reason).Scan(&logID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET status = 'executed',
		action_log_id = $2, decided_by = 1, decided_at = now() WHERE id = $1`,
		queueID, logID); err != nil {
		t.Fatal(err)
	}
	return queueID
}

func TestReadOutcome(t *testing.T) {
	pool, ctx := livePool(t)
	cases := []struct {
		outcome, reason, verdict string
		final                    bool
	}{
		{"success", "", "verified", true},
		{"rolled_back", "mean_exec_time regressed 41% (12.1 ms -> 17.1 ms)", "rolled_back", true},
		{"rollback_failed", "regressed 30%", "regressed", true},
		{"failed", "lock timeout", "failed", true},
		{"unverifiable", "no samples", "unverifiable", true},
		{"pending", "", "pending", false},
		{"monitoring", "", "pending", false},
	}
	for _, c := range cases {
		id := executedWithOutcome(t, ctx, pool, c.outcome, c.reason)
		o, err := ReadOutcome(ctx, pool, id)
		if err != nil || o.Final != c.final || o.Verdict != c.verdict {
			t.Errorf("%s: outcome = %+v, %v", c.outcome, o, err)
		}
		if c.reason != "" && !strings.Contains(o.Detail, c.reason) {
			t.Errorf("%s: detail %q lacks the numbers %q", c.outcome, o.Detail, c.reason)
		}
	}
}

func TestReadOutcomeOfUnexecutedItems(t *testing.T) {
	pool, ctx := livePool(t)
	pending, _ := queueOptimizerIndex(t, ctx, pool)
	if o, err := ReadOutcome(ctx, pool, pending); err != nil || o.Final {
		t.Fatalf("pending: %+v, %v", o, err)
	}
	for status, verdict := range map[string]string{"rejected": "rejected",
		"expired": "expired", "superseded": "superseded", "failed": "failed",
		"blocked": "blocked"} {
		id, _ := queueOptimizerIndex(t, ctx, pool)
		if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET status = $2,
			reason = 'because ' || $2 WHERE id = $1`, id, status); err != nil {
			t.Fatal(err)
		}
		o, err := ReadOutcome(ctx, pool, id)
		if err != nil || !o.Final || o.Verdict != verdict ||
			!strings.Contains(o.Detail, "because "+status) {
			t.Errorf("%s: %+v, %v", status, o, err)
		}
	}
	if _, err := ReadOutcome(ctx, pool, 987654321); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item: err = %v", err)
	}
}

func TestReadOutcomeUsesTheVerificationVerdict(t *testing.T) {
	pool, ctx := livePool(t)
	id := executedWithOutcome(t, ctx, pool, "success", "")
	var logID int64
	_ = pool.QueryRow(ctx, `SELECT action_log_id FROM sage.action_queue WHERE id = $1`,
		id).Scan(&logID)
	var decisionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('optimizer', 'x', '[]', 'execute', 'moderate', 'authorized', '{}',
		$1) RETURNING id`, fmt.Sprintf("ev-verify-%d", id)).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	var vID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.verification (decision_id,
		action_log_id, criterion, baseline, minimum_samples, next_evaluation_at,
		hard_deadline_at, verdict, reason, completed_at) VALUES ($1, $2, '{}', '{}', 1,
		now(), now(), 'success', 'p95 of 3 target queries 12.1 ms -> 3.4 ms', now())
		RETURNING id`, decisionID, logID).Scan(&vID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET verification_id = $2
		WHERE id = $1`, logID, vID); err != nil {
		t.Fatal(err)
	}
	o, err := ReadOutcome(ctx, pool, id)
	if err != nil || o.Verdict != "verified" || !strings.Contains(o.Detail, "12.1 ms -> 3.4 ms") {
		t.Fatalf("outcome = %+v, %v", o, err)
	}
}

type fakeDeliveries struct {
	mu     sync.Mutex
	items  []chatops.CardDelivery
	marked map[int64]string
	err    error
}

func (f *fakeDeliveries) PendingFollowups(context.Context, int) ([]chatops.CardDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []chatops.CardDelivery
	for _, d := range f.items {
		if _, done := f.marked[d.ID]; !done {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeDeliveries) MarkFollowedUp(_ context.Context, id int64, verdict string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.marked == nil {
		f.marked = map[int64]string{}
	}
	f.marked[id] = verdict
	return nil
}

type sentEvent struct {
	channel int
	evt     notify.Event
}

type fakeChannels struct {
	mu   sync.Mutex
	sent []sentEvent
	err  error
}

func (f *fakeChannels) SendToChannel(_ context.Context, id int, evt notify.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentEvent{channel: id, evt: evt})
	return nil
}

func TestFollowupsPostTheVerdictOnce(t *testing.T) {
	pool, ctx := livePool(t)
	verified := executedWithOutcome(t, ctx, pool, "success", "")
	rolled := executedWithOutcome(t, ctx, pool, "rolled_back", "regressed 41%")
	waiting := executedWithOutcome(t, ctx, pool, "monitoring", "")
	store := &fakeDeliveries{items: []chatops.CardDelivery{
		{ID: 1, ChannelID: 11, Database: "orders", QueueID: verified, Title: "Index A",
			Summary: "42.5% faster", MessageID: 9001, CreatedAt: time.Now()},
		{ID: 2, ChannelID: 12, Database: "orders", QueueID: rolled, Title: "Index B",
			CreatedAt: time.Now()},
		{ID: 3, ChannelID: 11, Database: "orders", QueueID: waiting, Title: "Index C",
			CreatedAt: time.Now()},
	}}
	chans := &fakeChannels{}
	f := &Followups{Store: store, Sender: chans,
		Pools: func(db string) *pgxpool.Pool {
			if db == "orders" {
				return pool
			}
			return nil
		}}
	n, err := f.RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("run = %d, %v", n, err)
	}
	if len(chans.sent) != 2 || store.marked[1] != "verified" ||
		store.marked[2] != "rolled_back" {
		t.Fatalf("sent %+v marked %v", chans.sent, store.marked)
	}
	first := chans.sent[0]
	if first.channel != 11 || first.evt.Data[notify.DataReplyTo] != int64(9001) ||
		!strings.Contains(first.evt.Subject, "Verified") ||
		!strings.Contains(first.evt.Body, "42.5% faster") {
		t.Fatalf("follow-up = %+v", first)
	}
	if !strings.Contains(chans.sent[1].evt.Body, "regressed 41%") {
		t.Fatalf("rollback follow-up = %+v", chans.sent[1].evt)
	}
	if n, err := f.RunOnce(ctx); err != nil || n != 0 || len(chans.sent) != 2 {
		t.Fatalf("second run sent again: %d, %v, %d", n, err, len(chans.sent))
	}
}

func TestFollowupsRetryOnSendFailureAndCloseOrphans(t *testing.T) {
	pool, ctx := livePool(t)
	verified := executedWithOutcome(t, ctx, pool, "success", "")
	store := &fakeDeliveries{items: []chatops.CardDelivery{
		{ID: 1, ChannelID: 11, Database: "orders", QueueID: verified, CreatedAt: time.Now()},
		{ID: 2, ChannelID: 11, Database: "gone", QueueID: 5, CreatedAt: time.Now()},
		{ID: 3, ChannelID: 11, Database: "orders", QueueID: 987654321,
			CreatedAt: time.Now()},
		{ID: 4, ChannelID: 11, Database: "orders", QueueID: verified,
			CreatedAt: time.Now().Add(-30 * 24 * time.Hour)},
	}}
	chans := &fakeChannels{err: errors.New("slack 503")}
	f := &Followups{Store: store, Sender: chans, GiveUp: 14 * 24 * time.Hour,
		Pools: func(db string) *pgxpool.Pool {
			if db == "orders" {
				return pool
			}
			return nil
		}}
	n, err := f.RunOnce(ctx)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "slack 503") {
		t.Fatalf("run = %d, %v", n, err)
	}
	if _, ok := store.marked[1]; ok {
		t.Fatal("a failed follow-up was marked as sent")
	}
	if store.marked[2] != "database_removed" || store.marked[3] != "queue_item_missing" ||
		store.marked[4] != "timed_out" {
		t.Fatalf("orphans marked %v", store.marked)
	}
	chans.err = nil
	if n, err := f.RunOnce(ctx); err != nil || n != 1 || store.marked[1] != "verified" {
		t.Fatalf("retry = %d, %v, %v", n, err, store.marked)
	}
}

func TestFollowupsStoreError(t *testing.T) {
	f := &Followups{Store: &fakeDeliveries{err: errors.New("control db down")},
		Sender: &fakeChannels{}, Pools: func(string) *pgxpool.Pool { return nil }}
	if _, err := f.RunOnce(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "control db down") {
		t.Fatalf("err = %v", err)
	}
	var nilF *Followups
	if _, err := nilF.RunOnce(context.Background()); err == nil {
		t.Fatal("a nil follow-up worker ran")
	}
}
