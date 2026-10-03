package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// Approval cards in chat: every queued action type gets Approve / Reject /
// Snooze buttons carrying a single-use, expiring card token bound to one
// channel and to the card's content hash. A callback must be signed by
// the provider, come from a chat user mapped to an operator or admin, and
// name a card that is unused, unexpired and unchanged; the decision then
// runs through the same approval path as the UI, once.

type chatCardFixture struct {
	*cardFixture
	h        http.Handler
	cards    *chatops.CardStore
	slackCh  int
	tgCh     int
	operator int
	viewer   int
	suffix   string
	replies  *replyLog
}

func newChatCardFixture(t *testing.T) *chatCardFixture {
	t.Helper()
	fx := newCardFixture(t)
	ctx := context.Background()
	cf := &chatCardFixture{cardFixture: fx, cards: chatops.NewCardStore(fx.pool),
		replies: &replyLog{}, suffix: strconv.FormatInt(time.Now().UnixNano(), 10)}
	ns := store.NewNotificationStore(fx.pool, nil)
	var err error
	if cf.slackCh, err = ns.CreateChannel(ctx, "card-slack-"+cf.suffix, "slack",
		map[string]string{"webhook_url": "https://hooks.slack.com/services/T/B/x",
			"interactive": "true", "signing_secret": testSigningSecret,
			"team_id": "T0001"}, 1); err != nil {
		t.Fatal(err)
	}
	if cf.tgCh, err = ns.CreateChannel(ctx, "card-tg-"+cf.suffix, "telegram",
		map[string]string{"bot_token": "123456:ABC", "chat_id": "-1001",
			"webhook_secret": "tg-secret_1"}, 1); err != nil {
		t.Fatal(err)
	}
	cf.operator = chatUser(t, fx.pool, auth.RoleOperator)
	cf.viewer = chatUser(t, fx.pool, auth.RoleViewer)
	ids := chatops.NewStore(fx.pool)
	for _, id := range []chatops.Identity{
		{Provider: chatops.ProviderSlack, TeamID: "T0001", ExternalUserID: "U-OP" + cf.suffix,
			UserID: cf.operator},
		{Provider: chatops.ProviderSlack, TeamID: "T0001", ExternalUserID: "U-VW" + cf.suffix,
			UserID: cf.viewer},
		{Provider: chatops.ProviderTelegram, ExternalUserID: cf.suffix, UserID: cf.operator},
	} {
		if _, err := ids.Link(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	restoreRun, restoreReply := chatopsRun, chatopsReply
	chatopsRun = func(fn func()) { fn() }
	chatopsReply = cf.replies.record
	t.Cleanup(func() { chatopsRun, chatopsReply = restoreRun, restoreReply })
	cf.h = NewRouterFullRuntime(fx.mgr, config.DefaultConfig(), fx.pool,
		&ActionDeps{Fleet: fx.mgr}, nil, nil, &RuntimeDeps{})
	return cf
}

// issue mints a card token for the fixture's queue item on a channel.
func (cf *chatCardFixture) issue(t *testing.T, channel int) string {
	t.Helper()
	a, err := store.NewActionStore(cf.pool).GetByID(context.Background(), cf.queueID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cf.cards.Issue(context.Background(), chatops.CardIssue{
		ChannelID: channel, Database: "orders", QueueID: cf.queueID,
		CardHash: approvalcard.ContentHash(*a), Title: "Stale statistics",
		ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (cf *chatCardFixture) slackPress(t *testing.T, user, actionID, token,
	secret string) (int, map[string]any) {
	t.Helper()
	body := slackBody(t, user, actionID, token)
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/slack/%d", cf.slackCh), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signSlack(req, body, secret, time.Now())
	return serveJSON(cf.h, req)
}

var tgUpdates = struct {
	mu sync.Mutex
	n  int64
}{n: time.Now().UnixNano() % 1e9}

func (cf *chatCardFixture) telegramPress(t *testing.T, from string, d chatops.Decision,
	token, secret string) (int, map[string]any) {
	t.Helper()
	tgUpdates.mu.Lock()
	tgUpdates.n++
	update := tgUpdates.n
	tgUpdates.mu.Unlock()
	fromID, _ := strconv.ParseInt(from, 10, 64)
	raw, _ := json.Marshal(map[string]any{"update_id": update,
		"callback_query": map[string]any{"id": "cb-" + strconv.FormatInt(update, 10),
			"from":    map[string]any{"id": fromID, "username": "bob"},
			"message": map[string]any{"message_id": 9001, "chat": map[string]any{"id": -1001}},
			"data":    chatops.CardCallbackData(d, token)}})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/telegram/%d", cf.tgCh), bytes.NewReader(raw))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	return serveJSON(cf.h, req)
}

func TestChatCardSlackApproveRunsOnceAndRecordsTheApprover(t *testing.T) {
	cf := newChatCardFixture(t)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["executed"] != true || body["database"] != "orders" {
		t.Fatalf("approve: %d %v", code, body)
	}
	var status string
	var decidedBy *int
	_ = cf.pool.QueryRow(context.Background(), `SELECT status, decided_by
		FROM sage.action_queue WHERE id = $1`, cf.queueID).Scan(&status, &decidedBy)
	if status != "executed" || decidedBy == nil || *decidedBy != cf.operator ||
		cf.executions(t) != 1 {
		t.Fatalf("queue %q by %v, executions %d", status, decidedBy, cf.executions(t))
	}
	d, err := cf.cards.Lookup(context.Background(), token)
	if err != nil || d.UsedBy == nil || *d.UsedBy != cf.operator || d.Decision != "approve" {
		t.Fatalf("card record = %+v, %v", d, err)
	}
	if reply := cf.replies.last(); reply == "" {
		t.Fatal("no reply in chat")
	}
	// The same card pressed again (a new delivery, so not a nonce replay).
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusConflict || body["code"] != "card_used" || cf.executions(t) != 1 {
		t.Fatalf("replayed card: %d %v, executions %d", code, body, cf.executions(t))
	}
}

func TestChatCardTelegramApprove(t *testing.T) {
	cf := newChatCardFixture(t)
	token := cf.issue(t, cf.tgCh)
	code, body := cf.telegramPress(t, cf.suffix, chatops.DecisionApprove, token, "tg-secret_1")
	if code != http.StatusOK || body["executed"] != true || cf.executions(t) != 1 {
		t.Fatalf("approve: %d %v, executions %d", code, body, cf.executions(t))
	}
	d, _ := cf.cards.Lookup(context.Background(), token)
	if d.MessageID != 9001 {
		t.Fatalf("the chat message of the decision was not kept: %+v", d)
	}
}

func TestChatCardForgedAndBadSignatures(t *testing.T) {
	cf := newChatCardFixture(t)
	token := cf.issue(t, cf.slackCh)
	cases := []struct {
		name  string
		token string
		sec   string
		want  int
		code  string
	}{
		{"forged token", "ZZZZZZZZZZZZZZZZZZZZZZ", testSigningSecret, http.StatusNotFound,
			"unknown_card"},
		{"bad signature", token, "not-the-secret", http.StatusUnauthorized, "bad_signature"},
	}
	for _, c := range cases {
		code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction,
			c.token, c.sec)
		if code != c.want || body["code"] != c.code {
			t.Errorf("%s: %d %v", c.name, code, body)
		}
	}
	if code, body := cf.telegramPress(t, cf.suffix, chatops.DecisionApprove, token,
		"wrong"); code != http.StatusUnauthorized {
		t.Errorf("telegram bad secret: %d %v", code, body)
	}
	if cf.executions(t) != 0 {
		t.Fatal("a forged or unsigned callback executed the action")
	}
	if d, _ := cf.cards.Lookup(context.Background(), token); d.UsedAt != nil {
		t.Fatal("a refused callback consumed the card")
	}
}

func TestChatCardExpired(t *testing.T) {
	cf := newChatCardFixture(t)
	token := cf.issue(t, cf.slackCh)
	if _, err := cf.pool.Exec(context.Background(), `UPDATE sage.approval_card_deliveries
		SET expires_at = now() - interval '1 second' WHERE queue_id = $1 AND channel_id = $2`,
		cf.queueID, cf.slackCh); err != nil {
		t.Fatal(err)
	}
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusGone || body["code"] != "card_expired" || cf.executions(t) != 0 {
		t.Fatalf("expired card: %d %v", code, body)
	}
}

func TestChatCardUnauthorisedUsersDoNotBurnTheCard(t *testing.T) {
	cf := newChatCardFixture(t)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-VW"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusForbidden {
		t.Fatalf("viewer: %d %v", code, body)
	}
	code, body = cf.slackPress(t, "U-NOBODY"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusForbidden || body["code"] != "unmapped_user" {
		t.Fatalf("unmapped: %d %v", code, body)
	}
	if cf.executions(t) != 0 {
		t.Fatal("an unauthorised user executed the action")
	}
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusOK || cf.executions(t) != 1 {
		t.Fatalf("the operator could not use the card afterwards: %d %v", code, body)
	}
}

func TestChatCardContentChangedAfterSend(t *testing.T) {
	cf := newChatCardFixture(t)
	token := cf.issue(t, cf.slackCh)
	if _, err := cf.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET proposed_sql = proposed_sql || ' ' WHERE id = $1`, cf.queueID); err != nil {
		t.Fatal(err)
	}
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusConflict || body["code"] != "content_changed" {
		t.Fatalf("changed content: %d %v", code, body)
	}
	var status string
	_ = cf.pool.QueryRow(context.Background(), `SELECT status FROM sage.action_queue
		WHERE id = $1`, cf.queueID).Scan(&status)
	if status != "pending" || cf.executions(t) != 0 {
		t.Fatalf("status %q, executions %d", status, cf.executions(t))
	}
}

func TestChatCardTokenBoundToItsChannel(t *testing.T) {
	cf := newChatCardFixture(t)
	tgToken := cf.issue(t, cf.tgCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, tgToken,
		testSigningSecret)
	if code != http.StatusForbidden || body["code"] != "wrong_channel" || cf.executions(t) != 0 {
		t.Fatalf("token from another channel: %d %v", code, body)
	}
}

func TestChatCardRejectAndSnooze(t *testing.T) {
	cf := newChatCardFixture(t)
	snoozeToken := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardSnoozeAction,
		snoozeToken, testSigningSecret)
	if code != http.StatusOK || body["status"] != "snoozed" {
		t.Fatalf("snooze: %d %v", code, body)
	}
	var until *time.Time
	var snoozeReason string
	_ = cf.pool.QueryRow(context.Background(), `SELECT snoozed_until,
		COALESCE(snooze_reason, '') FROM sage.action_queue WHERE id = $1`, cf.queueID).
		Scan(&until, &snoozeReason)
	if until == nil || time.Until(*until) < 3*time.Hour || snoozeReason == "" {
		t.Fatalf("snooze row: %v %q", until, snoozeReason)
	}
	rejectToken := cf.issue(t, cf.tgCh)
	code, body = cf.telegramPress(t, cf.suffix, chatops.DecisionDeny, rejectToken, "tg-secret_1")
	if code != http.StatusOK || body["status"] != "rejected" {
		t.Fatalf("reject: %d %v", code, body)
	}
	status, reason := cf.status(t)
	if status != "rejected" || reason == "" || cf.executions(t) != 0 {
		t.Fatalf("after reject: %q %q", status, reason)
	}
}

// Concurrent presses of one card by the operator (double click, two
// devices): exactly one execution.
func TestChatCardConcurrentPressesExecuteOnce(t *testing.T) {
	cf := newChatCardFixture(t)
	token := cf.issue(t, cf.slackCh)
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction,
				token, testSigningSecret)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 || cf.executions(t) != 1 {
		t.Fatalf("codes %v, executions %d", codes, cf.executions(t))
	}
}
