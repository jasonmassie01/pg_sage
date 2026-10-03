package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
)

// End to end through the chat path, with a fake Slack: the executor's
// approval request becomes a card with buttons, the operator presses
// Approve, the action runs exactly once through Executor.Apply, a replay
// is refused, and once verified the verdict is posted back to the chat.

type fakeSlack struct {
	mu       sync.Mutex
	payloads []map[string]any
}

func (f *fakeSlack) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.payloads = append(f.payloads, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeSlack) all() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.payloads...)
}

// oneChannelRules routes approval_needed to one channel only.
type oneChannelRules struct{ ch notify.Channel }

func (o *oneChannelRules) MatchingRules(_ context.Context, event string) ([]notify.Rule,
	error) {
	if event != "approval_needed" {
		return nil, nil
	}
	return []notify.Rule{{ID: 1, ChannelID: o.ch.ID, Event: event, MinSeverity: "info",
		Enabled: true}}, nil
}

func (o *oneChannelRules) Channel(_ context.Context, id int) (*notify.Channel, error) {
	if id != o.ch.ID {
		return nil, fmt.Errorf("channel %d not found", id)
	}
	ch := o.ch
	return &ch, nil
}

func (o *oneChannelRules) LogDelivery(context.Context, int, notify.Event, string,
	string) error {
	return nil
}

// cardButtonToken returns the card token on a Slack payload's Approve button.
func cardButtonToken(payload map[string]any) string {
	blocks, _ := payload["blocks"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		elems, _ := block["elements"].([]any)
		for _, e := range elems {
			m, _ := e.(map[string]any)
			if m["action_id"] == chatops.SlackCardApproveAction {
				v, _ := m["value"].(string)
				return v
			}
		}
	}
	return ""
}

func TestApprovalCardChatPathEndToEnd(t *testing.T) {
	cf := newChatCardFixture(t)
	ctx := context.Background()
	slack := &fakeSlack{}
	srv := slack.server(t)
	// Rules come from a store scoped to this test's channel, whose webhook is
	// the fake Slack: no other rule in the shared database can fire.
	dispatcher := notify.NewDispatcherWithStore(&oneChannelRules{ch: notify.Channel{
		ID: cf.slackCh, Name: "card-e2e", Type: "slack", Enabled: true,
		Config: map[string]string{"webhook_url": srv.URL, "interactive": "true",
			"signing_secret": testSigningSecret}}}, func(string, string, ...any) {})
	dispatcher.RegisterSender(notify.NewSlackSenderWithPolicy(
		notify.TargetPolicy{AllowPrivate: true}))
	dispatcher.WithCardTokens(approvalcard.TokenIssuer{Store: cf.cards})
	loader := approvalcard.Loader{Pool: cf.pool, Database: "orders", TrustLevel: "advisory"}
	notifier := approvalcard.NewNotifier(dispatcher, loader)

	// 1. The executor's approval request becomes a card with buttons.
	if err := notifier.Dispatch(ctx, notify.QueuedApprovalEvent("Stale statistics",
		cf.sql, "orders", "safe", cf.queueID)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	sent := slack.all()
	if len(sent) != 1 {
		t.Fatalf("slack received %d messages", len(sent))
	}
	token := cardButtonToken(sent[0])
	raw, _ := json.Marshal(sent[0])
	if !chatops.ValidCardToken(token) || !strings.Contains(string(raw), "Why it needs you") ||
		!strings.Contains(string(raw), cf.sql) {
		t.Fatalf("card message = %s", raw)
	}

	// 2. Approve in Slack: runs once, as the mapped operator.
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["executed"] != true || cf.executions(t) != 1 {
		t.Fatalf("approve: %d %v, executions %d", code, body, cf.executions(t))
	}
	// 3. Replays (same card, and the Telegram or UI path afterwards) never run it again.
	if code, _ := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret); code != http.StatusConflict {
		t.Fatalf("replay: %d", code)
	}
	if cf.executions(t) != 1 {
		t.Fatalf("executions after replay = %d", cf.executions(t))
	}

	// 4. Verified: the follow-up is posted to the same chat with the verdict.
	if _, err := cf.pool.Exec(ctx, `UPDATE sage.action_log SET outcome = 'success'
		WHERE sql_executed = $1`, cf.sql); err != nil {
		t.Fatal(err)
	}
	// Only this test's delivery: others in the shared database name other chats.
	if _, err := cf.pool.Exec(ctx, `DELETE FROM sage.approval_card_deliveries
		WHERE queue_id <> $1 OR channel_id <> $2`, cf.queueID, cf.slackCh); err != nil {
		t.Fatal(err)
	}
	f := &approvalcard.Followups{Store: cf.cards, Sender: dispatcher,
		Pools: func(db string) *pgxpool.Pool {
			if db == "orders" {
				return cf.pool
			}
			return nil
		}}
	if _, err := f.RunOnce(ctx); err != nil {
		t.Fatalf("follow-ups: %v", err)
	}
	sent = slack.all()
	last, _ := json.Marshal(sent[len(sent)-1])
	if len(sent) < 2 || !strings.Contains(string(last), "Verified") ||
		cardButtonToken(sent[len(sent)-1]) != "" {
		t.Fatalf("follow-up = %s (of %d messages)", last, len(sent))
	}
	before := len(sent)
	if _, err := f.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(slack.all()) != before {
		t.Fatal("the follow-up was posted twice")
	}
}
