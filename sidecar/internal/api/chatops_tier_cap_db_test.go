package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// ux-09a, decision D7: notification_policy.chat_approval_max_tier caps the
// risk tier a chat card may approve. A card above the cap is refused before
// its token is consumed, nothing runs, and the reply sends the person to
// the app. High is never approvable from chat. Each test presses the
// recorded Slack block_actions payload of the chat card tests (slackBody).
// Concurrent presses are covered by TestChatCardConcurrentPressesExecuteOnce;
// the cap check is a pure read before the consume.

// withChatCap rebuilds the fixture's router with the cap set.
func (cf *chatCardFixture) withChatCap(cap string) {
	cfg := config.DefaultConfig()
	cfg.NotificationPolicy.ChatApprovalMaxTier = cap
	cf.h = newTestRouter(cf.mgr, cfg, cf.pool, &ActionDeps{Fleet: cf.mgr}, nil, nil,
		&RuntimeDeps{})
}

// useRiskItem replaces the fixture's queue item with a pending ANALYZE of
// the stored risk tier (a high one is refused whatever its contract says).
func (cf *chatCardFixture) useRiskItem(t *testing.T, actionType, risk string) {
	t.Helper()
	id, err := store.NewActionStore(cf.pool).ProposeWithMetadata(context.Background(), nil,
		cf.findingID, cf.sql, "", risk, store.ActionProposalMetadata{ActionType: actionType,
			PolicyDecision: "queue_approval"})
	if err != nil {
		t.Fatal(err)
	}
	cf.queueID = id
}

func (cf *chatCardFixture) assertTierRefused(t *testing.T, token string, code int,
	body map[string]any, label string) {
	t.Helper()
	if code != http.StatusForbidden || body["code"] != "tier_cap" {
		t.Fatalf("%s: %d %v, want 403 tier_cap", label, code, body)
	}
	reply := cf.replies.last()
	if !strings.Contains(reply, "High-risk changes are approved in pg_sage") ||
		!strings.Contains(reply, "#/inbox") {
		t.Fatalf("%s: reply %q does not send the person to the app", label, reply)
	}
	var status string
	if err := cf.pool.QueryRow(context.Background(), `SELECT status FROM sage.action_queue
		WHERE id = $1`, cf.queueID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || cf.executions(t) != 0 {
		t.Fatalf("%s: queue %q, executions %d, want pending and nothing run", label, status,
			cf.executions(t))
	}
	if d, err := cf.cards.Lookup(context.Background(), token); err != nil || d.UsedAt != nil {
		t.Fatalf("%s: a refused card must keep its token: %+v, %v", label, d, err)
	}
}

func TestTierCap_ModerateCardIsApprovableWithTheDefault(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["status"] != "scheduled" {
		t.Fatalf("moderate with the default cap: %d %v, want 200 scheduled", code, body)
	}
}

func TestTierCap_ModerateCardIsRefusedWithSafe(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.useModerateItem(t, false)
	cf.withChatCap(config.ChatApprovalCapSafe)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	cf.assertTierRefused(t, token, code, body, "moderate under cap safe")
	// Reject is never capped: the person can still say no from chat.
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardRejectAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["status"] != "rejected" {
		t.Fatalf("reject under cap safe: %d %v", code, body)
	}
}

func TestTierCap_SafeCardFollowsTheCap(t *testing.T) {
	cf := newChatCardFixture(t)
	cf.withChatCap(config.ChatApprovalCapNone)
	token := cf.issue(t, cf.slackCh)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	cf.assertTierRefused(t, token, code, body, "safe under cap none")
	cf.withChatCap(config.ChatApprovalCapSafe)
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["executed"] != true || cf.executions(t) != 1 {
		t.Fatalf("safe under cap safe: %d %v, executions %d", code, body, cf.executions(t))
	}
}

// CHECK-02: a high-tier card is refused from chat with every configured
// value, on Slack and on Telegram, whether the tier comes from the stored
// risk or from the action's contract.
func TestTierCap_HighCardIsRefusedWithEveryCap(t *testing.T) {
	cf := newChatCardFixture(t)
	for _, item := range [][2]string{{"analyze_table", "high"},
		{"terminate_backend", "safe"}} {
		cf.useRiskItem(t, item[0], item[1])
		for _, cap := range []string{"", config.ChatApprovalCapSafe,
			config.ChatApprovalCapModerate, config.ChatApprovalCapNone} {
			cf.withChatCap(cap)
			label := item[0] + "/" + item[1] + " under cap " + cap
			token := cf.issue(t, cf.slackCh)
			code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackCardApproveAction,
				token, testSigningSecret)
			cf.assertTierRefused(t, token, code, body, "slack "+label)
			token = cf.issue(t, cf.tgCh)
			code, body = cf.telegramPress(t, cf.suffix, chatops.DecisionApprove, token,
				"tg-secret_1")
			cf.assertTierRefused(t, token, code, body, "telegram "+label)
		}
	}
}
