package approvalcard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/store"
)

// Sage SRE approvals go through the card notifier (policy-08): the
// approval event of an SRE queue item gets a card, a card token row in
// sage.approval_card_deliveries per interactive channel, a TTL of at most
// 15 minutes and no Snooze.

// ruleStore routes approval_needed to one interactive Slack channel.
type ruleStore struct {
	mu   sync.Mutex
	logs []string
}

func (r *ruleStore) MatchingRules(_ context.Context, typ string) ([]notify.Rule, error) {
	if typ != "approval_needed" {
		return nil, nil
	}
	return []notify.Rule{{ID: 1, ChannelID: 4711, Event: typ, MinSeverity: "info",
		Enabled: true}}, nil
}

func (r *ruleStore) Channel(_ context.Context, id int) (*notify.Channel, error) {
	return &notify.Channel{ID: id, Name: "ops", Type: "slack", Enabled: true,
		Config: map[string]string{"interactive": "true", "signing_secret": "s"}}, nil
}

func (r *ruleStore) LogDelivery(_ context.Context, _ int, _ notify.Event, status,
	errMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, status+errMsg)
	return nil
}

type slackCapture struct {
	mu     sync.Mutex
	events []notify.Event
}

func (s *slackCapture) Type() string { return "slack" }

func (s *slackCapture) Send(_ context.Context, _ notify.Channel, evt notify.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
	return nil
}

// queueSRECancel queues a Sage SRE cancel the way the action service does.
func queueSRECancel(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	ttl time.Duration) (sreaction.QueuedItem, sre.UUID) {
	t.Helper()
	id := sre.NewUUID()
	item, err := sreaction.NewPGApprovalQueue(pool, nil).Enqueue(ctx, sreaction.ApprovalItem{
		ProposalID: id, InvestigationID: sre.NewUUID(), PID: 5151,
		BackendStart: time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC),
		SQL:          "SELECT pg_cancel_backend(5151)", Title: "Cancel blocking backend pid 5151",
		Detail: sreSummary, Database: "orders", ExpiresAt: time.Now().Add(ttl)})
	if err != nil {
		t.Fatal(err)
	}
	return item, id
}

func TestNotifierIssuesABackendSignalCardForAnSREApproval(t *testing.T) {
	pool, ctx := livePool(t)
	item, proposal := queueSRECancel(t, ctx, pool, time.Hour)
	sender := &slackCapture{}
	d := notify.NewDispatcherWithStore(&ruleStore{}, func(string, string, ...any) {})
	d.RegisterSender(sender)
	d.WithCardTokens(TokenIssuer{Store: chatops.NewCardStore(pool)})
	n := NewNotifier(d, Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"})
	evt := notify.QueuedApprovalEvent("Cancel blocking backend pid 5151",
		"SELECT pg_cancel_backend(5151)", "orders", "moderate", item.QueueID)
	evt.Data["approval_proposal_id"] = string(proposal) // a pre-policy-08 producer
	sent := time.Now()
	if err := n.Dispatch(ctx, evt); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(sender.events) != 1 {
		t.Fatalf("slack events = %d", len(sender.events))
	}
	got := sender.events[0]
	ref, ok := notify.ApprovalCardOf(got)
	token, _ := got.Data[notify.DataCardToken].(string)
	if !ok || ref.QueueID != item.QueueID || !ref.NoSnooze ||
		ref.MaxTTL != BackendSignalCardTTL || !chatops.ValidCardToken(token) {
		t.Fatalf("card ref = %+v (%v), token %q", ref, ok, token)
	}
	var expires time.Time
	var hash string
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), expires_at, card_hash
		FROM sage.approval_card_deliveries WHERE queue_id = $1 AND database_name = 'orders'
		  AND channel_id = 4711`, item.QueueID).Scan(&rows, &expires, &hash); err != nil {
		t.Fatalf("card delivery row: %v", err)
	}
	if rows != 1 || expires.After(sent.Add(BackendSignalCardTTL+time.Second)) ||
		expires.Before(sent.Add(BackendSignalCardTTL-time.Minute)) {
		t.Fatalf("delivery rows %d expiring %v (sent %v), want one at most 15m", rows,
			expires, sent)
	}
	a, err := store.NewActionStore(pool).GetByID(ctx, item.QueueID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := CurrentHash(ctx, pool, *a)
	if err != nil || hash != ref.CardHash || current != hash {
		t.Fatalf("delivery hash %q, card %q, current %q (%v)", hash, ref.CardHash, current, err)
	}
}

// A queue item that expires sooner than 15 minutes bounds its card.
func TestBackendSignalCardEndsWithItsQueueItem(t *testing.T) {
	pool, ctx := livePool(t)
	item, _ := queueSRECancel(t, ctx, pool, 5*time.Minute)
	l := Loader{Pool: pool, Database: "orders"}
	c, err := l.Card(ctx, item.QueueID)
	if err != nil {
		t.Fatal(err)
	}
	ref := notify.CardRef{Database: "orders", QueueID: c.QueueID, CardHash: c.CardHash,
		Title: c.Title, ExpiresAt: c.ExpiresAt, MaxTTL: BackendSignalCardTTL}
	cards := chatops.NewCardStore(pool)
	token, err := TokenIssuer{Store: cards}.Issue(ctx, notify.Channel{ID: 8}, ref)
	if err != nil {
		t.Fatal(err)
	}
	d, err := cards.Lookup(ctx, token)
	if err != nil || d.ExpiresAt.After(c.ExpiresAt.Add(time.Second)) {
		t.Fatalf("delivery %+v (%v) outlives the queue item %v", d, err, c.ExpiresAt)
	}
}

func TestTokenIssuerMaxTTLBoundaries(t *testing.T) {
	pool, ctx := livePool(t)
	cards := chatops.NewCardStore(pool)
	base := notify.CardRef{Database: "orders", QueueID: 6, CardHash: "h", Title: "t",
		ExpiresAt: time.Now().Add(48 * time.Hour)}
	for _, tc := range []struct {
		name   string
		ttl    time.Duration
		maxTTL time.Duration
		want   time.Duration
	}{
		{"max ttl caps the issuer ttl", time.Hour, 15 * time.Minute, 15 * time.Minute},
		{"issuer ttl below max ttl wins", 10 * time.Minute, 15 * time.Minute,
			10 * time.Minute},
		{"zero max ttl keeps the issuer ttl", time.Hour, 0, time.Hour},
		{"negative max ttl is ignored", time.Hour, -time.Minute, time.Hour},
		{"zero issuer ttl uses the default, capped", 0, 15 * time.Minute, 15 * time.Minute},
	} {
		ref := base
		ref.MaxTTL = tc.maxTTL
		issued := time.Now()
		token, err := TokenIssuer{Store: cards, TTL: tc.ttl}.Issue(ctx,
			notify.Channel{ID: 3}, ref)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		d, err := cards.Lookup(ctx, token)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := d.ExpiresAt.Sub(issued); got > tc.want+time.Second ||
			got < tc.want-5*time.Second {
			t.Errorf("%s: card lives %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The live hash follows the finding's recorded signal target: a changed
// backend_start (a reused pid) is a changed card.
func TestCurrentHashFollowsTheSignalTarget(t *testing.T) {
	pool, ctx := livePool(t)
	item, _ := queueSRECancel(t, ctx, pool, time.Hour)
	as := store.NewActionStore(pool)
	a, err := as.GetByID(ctx, item.QueueID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Loader{Pool: pool, Database: "orders"}.Card(ctx, item.QueueID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := CurrentHash(ctx, pool, *a)
	if err != nil || before != c.CardHash {
		t.Fatalf("current %q (%v), card %q", before, err, c.CardHash)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET detail = jsonb_set(detail,
		'{signal_target,backend_start}', '"2026-11-09T09:00:00Z"') WHERE id = $1`,
		item.FindingID); err != nil {
		t.Fatal(err)
	}
	after, err := CurrentHash(ctx, pool, *a)
	if err != nil || after == before {
		t.Fatalf("hash after a new backend_start = %q (%v), was %q", after, err, before)
	}
	// A non-signal item's live hash is its content hash; no finding read.
	queueID, _ := queueOptimizerIndex(t, ctx, pool)
	idx, err := as.GetByID(ctx, queueID)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := CurrentHash(ctx, pool, *idx); err != nil || h != ContentHash(*idx) {
		t.Fatalf("index item hash = %q (%v), want ContentHash", h, err)
	}
	if _, err := CurrentHash(ctx, nil, *a); err == nil {
		t.Fatal("a signal item's hash was computed without a pool")
	}
}
