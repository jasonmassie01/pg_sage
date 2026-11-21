package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// ux-09a, decision D7: a card above the chat approval cap is sent with no
// Approve button (Reject and Snooze stay). No concurrent access tests: the
// payload builders are pure.

func cappedCardEvent(noApprove bool) Event {
	evt := WithApprovalCard(Event{Type: "approval_needed", Severity: "warning",
		Subject: "Approve?"}, CardRef{Database: "orders", QueueID: 7, NoApprove: noApprove},
		"card body")
	evt.Data[DataCardToken] = "tok-1"
	return evt
}

func slackButtonIDs(t *testing.T, evt Event) []string {
	t.Helper()
	payload, err := buildSlackPayload(evt)
	if err != nil {
		t.Fatal(err)
	}
	if payload, err = withCardButtons(payload, evt); err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Blocks []struct {
			Type     string `json:"type"`
			Elements []struct {
				ActionID string `json:"action_id"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range msg.Blocks {
		if b.Type == "actions" {
			for _, e := range b.Elements {
				ids = append(ids, e.ActionID)
			}
		}
	}
	return ids
}

func TestCardButtons_SlackOmitsApproveAboveTheCap(t *testing.T) {
	ids := strings.Join(slackButtonIDs(t, cappedCardEvent(false)), ",")
	if !strings.Contains(ids, chatops.SlackCardApproveAction) ||
		!strings.Contains(ids, chatops.SlackCardRejectAction) {
		t.Fatalf("an approvable card has buttons %s", ids)
	}
	ids = strings.Join(slackButtonIDs(t, cappedCardEvent(true)), ",")
	if strings.Contains(ids, chatops.SlackCardApproveAction) ||
		!strings.Contains(ids, chatops.SlackCardRejectAction) ||
		!strings.Contains(ids, chatops.SlackCardSnoozeAction) {
		t.Fatalf("a card above the cap has buttons %s, want Reject and Snooze only", ids)
	}
}

func TestCardButtons_TelegramOmitsApproveAboveTheCap(t *testing.T) {
	raw, _ := json.Marshal(telegramMessage(cappedCardEvent(true))["reply_markup"])
	if strings.Contains(string(raw), `"Approve"`) || !strings.Contains(string(raw), `"Reject"`) {
		t.Fatalf("telegram keyboard above the cap = %s", raw)
	}
	raw, _ = json.Marshal(telegramMessage(cappedCardEvent(false))["reply_markup"])
	if !strings.Contains(string(raw), `"Approve"`) {
		t.Fatalf("telegram keyboard of an approvable card = %s", raw)
	}
}
