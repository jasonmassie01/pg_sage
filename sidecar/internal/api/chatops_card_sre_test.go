package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/store"
)

// policy-08: one chat approval protocol. A Sage SRE cancel is decided
// through its approval card: the card is bound to the channel it was sent
// to, to its content and backend incarnation (pid and backend_start), and
// to a TTL of at most 15 minutes; it has no Snooze. The retired
// proposal-id buttons only get an "outdated" reply and change nothing.

// sreCard issues the fixture's SRE queue item card to a channel the way
// the card notifier does; it returns the token and when the card ends.
func (cf *chatFixture) sreCard(t *testing.T, channel int) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	c, err := approvalcard.Loader{Pool: cf.pool, Database: "orders"}.Card(ctx,
		cf.proposal.QueueID)
	if err != nil {
		t.Fatal(err)
	}
	cards := chatops.NewCardStore(cf.pool)
	token, err := approvalcard.TokenIssuer{Store: cards}.Issue(ctx,
		notify.Channel{ID: channel}, approvalcard.CardRefOf(c))
	if err != nil {
		t.Fatal(err)
	}
	d, err := cards.Lookup(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return token, d.ExpiresAt
}

// slackTo presses a Slack button posted to channel.
func (cf *chatFixture) slackTo(t *testing.T, channel int, actionID,
	value string) (int, map[string]any) {
	t.Helper()
	body := slackBody(t, cf.mustExternal(), actionID, value)
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/slack/%d", channel), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signSlack(req, body, testSigningSecret, time.Now())
	return serveJSON(cf.h, req)
}

func (cf *chatFixture) proposalState(t *testing.T) sreaction.ProposalState {
	t.Helper()
	p, err := cf.f.actions.Get(context.Background(), cf.proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p.State
}

func (cf *chatFixture) assertUntouched(t *testing.T, what string) {
	t.Helper()
	status, decided := cf.queue(t)
	if status != "pending" || decided != nil || cf.f.canceler.count() != 0 ||
		cf.proposalState(t) != sreaction.ProposalRequested {
		t.Fatalf("%s changed state: queue %s by %v, cancels %d, proposal %s", what,
			status, decided, cf.f.canceler.count(), cf.proposalState(t))
	}
}

func TestSRECardFromASecondSlackChannelIsWrongChannel(t *testing.T) {
	cf := newChatFixture(t)
	second, err := store.NewNotificationStore(cf.pool, nil).CreateChannel(
		context.Background(), "slack-2-"+cf.suffix(), "slack", map[string]string{
			"webhook_url": "https://hooks.slack.com/services/T/B/y", "interactive": "true",
			"signing_secret": testSigningSecret, "team_id": "T0001"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := cf.sreCard(t, cf.slackCh)
	code, out := cf.slackTo(t, second, chatops.SlackCardApproveAction, token)
	if code != http.StatusForbidden || out["code"] != "wrong_channel" {
		t.Fatalf("card from a second channel = %d %v, want 403 wrong_channel", code, out)
	}
	cf.assertUntouched(t, "a card pressed in another channel")
	d, _ := chatops.NewCardStore(cf.pool).Lookup(context.Background(), token)
	if d.UsedAt != nil {
		t.Fatal("a wrong-channel press consumed the card")
	}
}

// The card ends at the earlier of 15 minutes and the queue item's expiry:
// one second past it is refused, one second before it is accepted.
func TestSRECardTTLBoundary(t *testing.T) {
	cf := newChatFixture(t)
	issued := time.Now()
	token, expires := cf.sreCard(t, cf.slackCh)
	if ttl := expires.Sub(issued); ttl > approvalcard.BackendSignalCardTTL+time.Second ||
		ttl <= 0 {
		t.Fatalf("SRE card TTL = %v, want at most 15m", ttl)
	}
	queueExpiry := cf.proposal.ExpiresAt
	if expires.After(queueExpiry.Add(time.Second)) {
		t.Fatalf("card ends %v after its queue item %v", expires, queueExpiry)
	}
	restore := chatopsNow
	t.Cleanup(func() { chatopsNow = restore })
	chatopsNow = func() time.Time { return expires.Add(time.Second) }
	code, out := cf.slackTo(t, cf.slackCh, chatops.SlackCardApproveAction, token)
	if code != http.StatusGone || out["code"] != "card_expired" {
		t.Fatalf("at TTL+1s = %d %v, want 410 card_expired", code, out)
	}
	cf.assertUntouched(t, "an expired card")
	chatopsNow = func() time.Time { return expires.Add(-time.Second) }
	code, out = cf.slackTo(t, cf.slackCh, chatops.SlackCardApproveAction, token)
	if code != http.StatusOK || out["executed"] != true || cf.f.canceler.count() != 1 {
		t.Fatalf("at TTL-1s = %d %v, cancels %d", code, out, cf.f.canceler.count())
	}
}

// A reused pid: the backend the card showed is no longer the one the
// evidence names, so the card is refused as changed content.
func TestSRECardRefusedWhenTheBackendIncarnationChanged(t *testing.T) {
	cf := newChatFixture(t)
	token, _ := cf.sreCard(t, cf.slackCh)
	if _, err := cf.pool.Exec(context.Background(), `UPDATE sage.findings f
		SET detail = jsonb_set(f.detail, '{signal_target,backend_start}',
		    '"2026-10-01T09:05:00Z"')
		FROM sage.action_queue q WHERE q.id = $1 AND f.id = q.finding_id`,
		cf.proposal.QueueID); err != nil {
		t.Fatal(err)
	}
	code, out := cf.slackTo(t, cf.slackCh, chatops.SlackCardApproveAction, token)
	if code != http.StatusConflict || out["code"] != "content_changed" {
		t.Fatalf("changed backend_start = %d %v, want 409 content_changed", code, out)
	}
	cf.assertUntouched(t, "a card for another backend incarnation")
	if !strings.Contains(cf.replies.last(), "changed after the card was sent") {
		t.Fatalf("chat reply = %q", cf.replies.last())
	}
}

// A backend-signal card has no Snooze; a forged snooze press is refused
// without using the card, which still approves afterwards.
func TestSRECardSnoozeIsRefused(t *testing.T) {
	cf := newChatFixture(t)
	token, _ := cf.sreCard(t, cf.slackCh)
	code, out := cf.slackTo(t, cf.slackCh, chatops.SlackCardSnoozeAction, token)
	if code != http.StatusConflict || out["code"] != "snooze_unavailable" {
		t.Fatalf("snooze = %d %v, want 409 snooze_unavailable", code, out)
	}
	var snoozed *time.Time
	_ = cf.pool.QueryRow(context.Background(), `SELECT snoozed_until FROM sage.action_queue
		WHERE id = $1`, cf.proposal.QueueID).Scan(&snoozed)
	if snoozed != nil {
		t.Fatalf("the SRE item was snoozed until %v", snoozed)
	}
	cf.assertUntouched(t, "a refused snooze")
	code, out = cf.slackTo(t, cf.slackCh, chatops.SlackCardApproveAction, token)
	if code != http.StatusOK || out["executed"] != true {
		t.Fatalf("approve after a refused snooze = %d %v", code, out)
	}
}

// Happy path: the approval runs exactly the cancel the card showed, as
// the mapped operator, and the proposal ends executed.
func TestSRECardApproveRunsTheCardSQL(t *testing.T) {
	cf := newChatFixture(t)
	token, _ := cf.sreCard(t, cf.tgCh)
	code, out := cf.telegram(telegramBody(time.Now().UnixNano(), cf.suffix(), "-1001",
		chatops.CardCallbackData(chatops.DecisionApprove, token)), "tg-secret_1")
	if code != http.StatusOK || out["executed"] != true || out["queue_id"] !=
		float64(cf.proposal.QueueID) || out["database"] != "orders" {
		t.Fatalf("approve = %d %v", code, out)
	}
	if cf.f.canceler.count() != 1 {
		t.Fatalf("cancels = %d", cf.f.canceler.count())
	}
	call := cf.f.canceler.calls[0]
	if call.Target.PID != cf.proposal.Target.PID || call.ApprovedBy != cf.operator ||
		!call.Target.BackendStart.Equal(cf.proposal.Target.BackendStart) {
		t.Fatalf("cancel = %+v, want pid %d approved by %d", call, cf.proposal.Target.PID,
			cf.operator)
	}
	var sql string
	_ = cf.pool.QueryRow(context.Background(), `SELECT proposed_sql FROM sage.action_queue
		WHERE id = $1`, cf.proposal.QueueID).Scan(&sql)
	if sql != fmt.Sprintf("SELECT pg_cancel_backend(%d)", cf.proposal.Target.PID) {
		t.Fatalf("queue SQL = %q", sql)
	}
	if cf.proposalState(t) != sreaction.ProposalExecuted {
		t.Fatalf("proposal = %s, want executed", cf.proposalState(t))
	}
}

// Two presses of one SRE card at once (double click, two devices): one
// cancel.
func TestSRECardConcurrentPressesCancelOnce(t *testing.T) {
	cf := newChatFixture(t)
	token, _ := cf.sreCard(t, cf.slackCh)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = cf.slackTo(t, cf.slackCh, chatops.SlackCardApproveAction, token)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 || cf.f.canceler.count() != 1 {
		t.Fatalf("codes %v, cancels %d", codes, cf.f.canceler.count())
	}
}

// CHECK-03: an old-format button changes no queue state; it is answered
// as outdated, after the usual authentication and replay checks.
func TestOutdatedProposalButtonsChangeNothing(t *testing.T) {
	cf := newChatFixture(t)
	const want = "This button is outdated; open pg_sage to decide."
	for _, action := range []string{chatops.LegacySlackApproveAction,
		chatops.LegacySlackDenyAction} {
		code, out := cf.slackTo(t, cf.slackCh, action, string(cf.proposal.ID))
		if code != http.StatusGone || out["code"] != "outdated_button" {
			t.Fatalf("%s = %d %v, want 410 outdated_button", action, code, out)
		}
		if cf.replies.last() != want {
			t.Fatalf("%s reply = %q", action, cf.replies.last())
		}
	}
	update := time.Now().UnixNano()
	for i, data := range []string{"sage:a:" + string(cf.proposal.ID),
		"sage:d:" + string(cf.proposal.ID)} {
		code, out := cf.telegram(telegramBody(update+int64(i), cf.suffix(), "-1001", data),
			"tg-secret_1")
		if code != http.StatusGone || out["code"] != "outdated_button" {
			t.Fatalf("telegram %s = %d %v", data, code, out)
		}
	}
	cf.assertUntouched(t, "an outdated button")
	// The checks before it are unchanged: an unmapped user is still refused.
	body := slackBody(t, "U-STRANGER", chatops.LegacySlackApproveAction,
		string(cf.proposal.ID))
	if code, out := cf.slack(t, body, testSigningSecret, time.Now()); code !=
		http.StatusForbidden || out["code"] != "unmapped_user" {
		t.Fatalf("unmapped user, outdated button = %d %v", code, out)
	}
	if code, out := cf.slack(t, body, "not-the-secret", time.Now()); code !=
		http.StatusUnauthorized || out["code"] != "bad_signature" {
		t.Fatalf("forged outdated button = %d %v", code, out)
	}
	cf.assertUntouched(t, "a refused outdated button")
}

// A cancel queued without an action_type is still a backend signal: a
// forged Snooze press on its card is refused and changes nothing.
func TestUntypedSignalCardSnoozeIsRefused(t *testing.T) {
	cf := newChatFixture(t)
	if _, err := cf.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET action_type = NULL WHERE id = $1`, cf.proposal.QueueID); err != nil {
		t.Fatal(err)
	}
	token, _ := cf.sreCard(t, cf.slackCh)
	code, out := cf.slackTo(t, cf.slackCh, chatops.SlackCardSnoozeAction, token)
	if code != http.StatusConflict || out["code"] != "snooze_unavailable" {
		t.Fatalf("snooze of an untyped cancel = %d %v, want 409 snooze_unavailable",
			code, out)
	}
	cf.assertUntouched(t, "a refused snooze of an untyped cancel")
}
