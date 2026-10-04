package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/facts"
)

// Fact cards in chat (roadmap 2.3) reuse the approval-card token rules: a
// signed callback from a mapped operator, a token issued to this channel,
// unused and unexpired, naming a fact that has not changed since the card
// was sent. The decision is recorded with the operator as the decider.

func (cf *chatCardFixture) proposeFact(t *testing.T) facts.Fact {
	t.Helper()
	_, _ = cf.pool.Exec(context.Background(), "DELETE FROM sage.facts")
	t.Cleanup(func() { _, _ = cf.pool.Exec(context.Background(), "DELETE FROM sage.facts") })
	f, _, err := facts.NewStore(cf.pool).Propose(context.Background(), facts.Proposal{
		Type: facts.TypeAppMigrations, Kind: facts.KindIndex,
		Subject: "public.idx_thesis_run", Source: facts.SourceDetector,
		Evidence: []facts.Citation{{Kind: "action_log", Ref: "drop_index:public.idx_thesis_run",
			Detail: "dropped 8 times, came back"}}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (cf *chatCardFixture) issueFact(t *testing.T, channel int, f facts.Fact) string {
	t.Helper()
	token, err := chatops.NewFactCardStore(cf.pool).Issue(context.Background(),
		chatops.FactCardIssue{ChannelID: channel, Database: "orders", FactID: f.ID,
			FactHash: f.Hash(), Title: f.Describe(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (cf *chatCardFixture) telegramFactPress(t *testing.T, d chatops.Decision,
	token string) (int, map[string]any) {
	t.Helper()
	tgUpdates.mu.Lock()
	tgUpdates.n++
	update := tgUpdates.n
	tgUpdates.mu.Unlock()
	fromID, _ := strconv.ParseInt(cf.suffix, 10, 64)
	raw, _ := json.Marshal(map[string]any{"update_id": update,
		"callback_query": map[string]any{"id": "cb-" + strconv.FormatInt(update, 10),
			"from":    map[string]any{"id": fromID, "username": "bob"},
			"message": map[string]any{"message_id": 9002, "chat": map[string]any{"id": -1001}},
			"data":    chatops.FactCallbackData(d, token)}})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/telegram/%d", cf.tgCh), bytes.NewReader(raw))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret_1")
	return serveJSON(cf.h, req)
}

func TestChatFactConfirmOnceFromSlack(t *testing.T) {
	cf := newChatCardFixture(t)
	f := cf.proposeFact(t)
	token := cf.issueFact(t, cf.slackCh, f)
	code, body := cf.slackPress(t, "U-VW"+cf.suffix, chatops.SlackFactConfirmAction, token,
		testSigningSecret)
	if code != http.StatusForbidden {
		t.Fatalf("a viewer confirmed a fact: %d %v", code, body)
	}
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackFactConfirmAction, token,
		testSigningSecret)
	if code != http.StatusOK || body["status"] != "confirmed" || body["fact_id"] !=
		float64(f.ID) {
		t.Fatalf("confirm: %d %v", code, body)
	}
	got, _ := facts.NewStore(cf.pool).Get(context.Background(), f.ID)
	if got.Status != facts.StatusConfirmed || got.DecidedBy == "" {
		t.Fatalf("fact after confirm: %+v", got)
	}
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackFactRejectAction, token,
		testSigningSecret)
	if code != http.StatusConflict || body["code"] != "card_used" {
		t.Fatalf("reused fact card: %d %v", code, body)
	}
}

func TestChatFactRejectFromTelegramAndStaleCards(t *testing.T) {
	cf := newChatCardFixture(t)
	f := cf.proposeFact(t)
	token := cf.issueFact(t, cf.tgCh, f)
	code, body := cf.telegramFactPress(t, chatops.DecisionDeny, token)
	if code != http.StatusOK || body["status"] != "rejected" {
		t.Fatalf("reject: %d %v", code, body)
	}
	stale := cf.issueFact(t, cf.slackCh, f) // hash of the proposed fact, now rejected
	f2, _ := facts.NewStore(cf.pool).Get(context.Background(), f.ID)
	if f2.Hash() == f.Hash() {
		t.Fatal("the hash must change with the decision")
	}
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackFactConfirmAction, stale,
		testSigningSecret)
	if code != http.StatusConflict || body["code"] != "content_changed" {
		t.Fatalf("stale card: %d %v", code, body)
	}
	wrong := cf.issueFact(t, cf.tgCh, f2)
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackFactConfirmAction, wrong,
		testSigningSecret)
	if code != http.StatusForbidden || body["code"] != "wrong_channel" {
		t.Fatalf("card from another channel: %d %v", code, body)
	}
}
