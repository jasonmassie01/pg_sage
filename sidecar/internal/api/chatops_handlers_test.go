package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/store"
)

// ChatOps approval (AI-SRE-SPEC R1.1): Slack and Telegram callbacks are
// verified (Slack v0 HMAC with a timestamp tolerance; Telegram's per-bot
// secret token), processed once, and attributed to the pg_sage user the
// chat user is explicitly mapped to. Approval goes through the existing
// approval path (single use, decided_by = the mapped user); unmapped
// users and viewers are refused.

const testSigningSecret = "8f742231b10e8888abcd99yyyzzz85a5"

type replyLog struct {
	mu      sync.Mutex
	replies []string
}

func (l *replyLog) record(_ context.Context, _ notify.Channel, _ chatops.Action,
	text string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.replies = append(l.replies, text)
	return nil
}

func (l *replyLog) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.replies) == 0 {
		return ""
	}
	return l.replies[len(l.replies)-1]
}

type chatFixture struct {
	h        http.Handler
	pool     *pgxpool.Pool
	f        *actionFixture
	proposal sreaction.Proposal
	slackCh  int
	tgCh     int
	operator int
	viewer   int
	replies  *replyLog
}

// newChatFixture: one requested proposal, a Slack and a Telegram channel,
// an operator mapped on both and a viewer mapped on Slack.
func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	mgr := fleet.NewManager(config.DefaultConfig())
	f := actionInstance(t, mgr, "orders", "advisory")
	ctx := context.Background()
	p, err := f.actions.Propose(ctx, f.inv.ID, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	if p, err = f.actions.RequestExecution(ctx, p.ID, "user:1"); err != nil {
		t.Fatal(err)
	}
	cf := &chatFixture{pool: f.pool, f: f, proposal: p, replies: &replyLog{}}
	ns := store.NewNotificationStore(f.pool, nil)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	if cf.slackCh, err = ns.CreateChannel(ctx, "slack-"+suffix, "slack",
		map[string]string{"webhook_url": "https://hooks.slack.com/services/T/B/x",
			"interactive": "true", "signing_secret": testSigningSecret,
			"team_id": "T0001"}, 1); err != nil {
		t.Fatal(err)
	}
	if cf.tgCh, err = ns.CreateChannel(ctx, "tg-"+suffix, "telegram",
		map[string]string{"bot_token": "123456:ABC", "chat_id": "-1001",
			"webhook_secret": "tg-secret_1"}, 1); err != nil {
		t.Fatal(err)
	}
	cf.operator = chatUser(t, f.pool, auth.RoleOperator)
	cf.viewer = chatUser(t, f.pool, auth.RoleViewer)
	ids := chatops.NewStore(f.pool)
	for _, id := range []chatops.Identity{
		{Provider: chatops.ProviderSlack, TeamID: "T0001", ExternalUserID: "U-OP" + suffix,
			UserID: cf.operator},
		{Provider: chatops.ProviderSlack, TeamID: "T0001", ExternalUserID: "U-VW" + suffix,
			UserID: cf.viewer},
		{Provider: chatops.ProviderTelegram, ExternalUserID: suffix, UserID: cf.operator},
	} {
		if _, err := ids.Link(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	restoreRun, restoreReply := chatopsRun, chatopsReply
	chatopsRun = func(fn func()) { fn() }
	chatopsReply = cf.replies.record
	t.Cleanup(func() { chatopsRun, chatopsReply = restoreRun, restoreReply })
	cf.h = NewRouterFullRuntime(mgr, config.DefaultConfig(), f.pool,
		&ActionDeps{Fleet: mgr}, nil, nil, &RuntimeDeps{})
	return cf
}

func chatUser(t *testing.T, pool *pgxpool.Pool, role string) int {
	t.Helper()
	id, err := auth.CreateUser(context.Background(), pool, fmt.Sprintf("%s-%d@chat.test",
		role, time.Now().UnixNano()), "correct horse battery", role)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (cf *chatFixture) suffix() string {
	return strings.TrimPrefix(cf.mustExternal(), "U-OP")
}

func (cf *chatFixture) mustExternal() string {
	var ext string
	_ = cf.pool.QueryRow(context.Background(), `SELECT external_user_id
		FROM sage.chatops_identities WHERE user_id = $1 AND provider = 'slack'`,
		cf.operator).Scan(&ext)
	return ext
}

func slackBody(t *testing.T, user, actionID, proposal string) []byte {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"type": "block_actions",
		"team":         map[string]any{"id": "T0001"},
		"user":         map[string]any{"id": user, "username": "alice"},
		"response_url": "https://hooks.slack.com/actions/T0001/1/abc",
		"actions": []any{map[string]any{"action_id": actionID, "value": proposal,
			"action_ts": strconv.FormatInt(time.Now().UnixNano(), 10)}}})
	return []byte(url.Values{"payload": {string(raw)}}.Encode())
}

func signSlack(req *http.Request, body []byte, secret string, at time.Time) {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
}

func (cf *chatFixture) slack(t *testing.T, body []byte, secret string,
	at time.Time) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/slack/%d", cf.slackCh), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signSlack(req, body, secret, at)
	return serveJSON(cf.h, req)
}

func serveJSON(h http.Handler, req *http.Request) (int, map[string]any) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (cf *chatFixture) queue(t *testing.T) (string, *int) {
	t.Helper()
	var status string
	var decided *int
	if err := cf.pool.QueryRow(context.Background(), `SELECT status, decided_by
		FROM sage.action_queue WHERE id = $1`, cf.proposal.QueueID).
		Scan(&status, &decided); err != nil {
		t.Fatal(err)
	}
	return status, decided
}

func TestChatOpsSlackApprovalRunsTheExistingApproval(t *testing.T) {
	cf := newChatFixture(t)
	body := slackBody(t, cf.mustExternal(), chatops.SlackApproveAction, string(cf.proposal.ID))
	code, out := cf.slack(t, body, testSigningSecret, time.Now())
	if code != 200 || out["decision"] != "approve" {
		t.Fatalf("slack approve = %d %v", code, out)
	}
	if cf.f.canceler.count() != 1 || cf.f.canceler.calls[0].ApprovedBy != cf.operator {
		t.Fatalf("cancels = %+v, want one approved by user %d", cf.f.canceler.calls,
			cf.operator)
	}
	status, decided := cf.queue(t)
	if status != "approved" || decided == nil || *decided != cf.operator {
		t.Fatalf("queue = %s decided by %v", status, decided)
	}
	p, _ := cf.f.actions.Get(context.Background(), cf.proposal.ID)
	if p.State != sreaction.ProposalExecuted || p.DecidedBy != cf.operator {
		t.Fatalf("proposal = %s by %d", p.State, p.DecidedBy)
	}
	if !strings.Contains(cf.replies.last(), "executed") {
		t.Fatalf("chat reply = %q", cf.replies.last())
	}
	// The exact same delivery again is a replay.
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/slack/%d", cf.slackCh), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signSlack(req, body, testSigningSecret, time.Now())
	if code, out := serveJSON(cf.h, req); code != http.StatusConflict ||
		out["code"] != "replay" {
		t.Fatalf("replayed callback = %d %v", code, out)
	}
	if cf.f.canceler.count() != 1 {
		t.Fatal("a replayed callback executed again")
	}
}

func TestChatOpsSlackRefusesForgedAndStaleCallbacks(t *testing.T) {
	cf := newChatFixture(t)
	for name, tc := range map[string]struct {
		secret string
		at     time.Time
	}{
		"wrong secret": {"not-the-secret", time.Now()},
		"stale":        {testSigningSecret, time.Now().Add(-6 * time.Minute)},
		"future":       {testSigningSecret, time.Now().Add(6 * time.Minute)},
	} {
		body := slackBody(t, cf.mustExternal(), chatops.SlackApproveAction,
			string(cf.proposal.ID))
		if code, out := cf.slack(t, body, tc.secret, tc.at); code !=
			http.StatusUnauthorized || out["code"] != "bad_signature" {
			t.Errorf("%s = %d %v, want 401 bad_signature", name, code, out)
		}
	}
	if status, _ := cf.queue(t); status != "pending" || cf.f.canceler.count() != 0 {
		t.Fatalf("a forged callback changed the queue (%s) or executed", status)
	}
}

func TestChatOpsSlackRefusesUnmappedAndViewerUsers(t *testing.T) {
	cf := newChatFixture(t)
	viewerExt := "U-VW" + cf.suffix()
	for name, tc := range map[string]struct {
		user, code string
	}{
		"unmapped": {"U-STRANGER", "unmapped_user"},
		"viewer":   {viewerExt, "forbidden"},
	} {
		body := slackBody(t, tc.user, chatops.SlackApproveAction, string(cf.proposal.ID))
		if code, out := cf.slack(t, body, testSigningSecret, time.Now()); code !=
			http.StatusForbidden || out["code"] != tc.code {
			t.Errorf("%s = %d %v, want 403 %s", name, code, out, tc.code)
		}
	}
	if status, _ := cf.queue(t); status != "pending" || cf.f.canceler.count() != 0 {
		t.Fatalf("a refused user changed the queue (%s)", status)
	}
}

func TestChatOpsSlackDenyRejectsAndAttributes(t *testing.T) {
	cf := newChatFixture(t)
	body := slackBody(t, cf.mustExternal(), chatops.SlackDenyAction, string(cf.proposal.ID))
	if code, out := cf.slack(t, body, testSigningSecret, time.Now()); code != 200 ||
		out["decision"] != "deny" {
		t.Fatalf("deny = %d %v", code, out)
	}
	status, decided := cf.queue(t)
	if status != "rejected" || decided == nil || *decided != cf.operator {
		t.Fatalf("queue = %s decided by %v", status, decided)
	}
	p, _ := cf.f.actions.Get(context.Background(), cf.proposal.ID)
	if p.State != sreaction.ProposalDenied || p.DecidedBy != cf.operator ||
		cf.f.canceler.count() != 0 {
		t.Fatalf("proposal = %s by %d, cancels %d", p.State, p.DecidedBy,
			cf.f.canceler.count())
	}
}

func TestChatOpsSlackChannelAndProposalChecks(t *testing.T) {
	cf := newChatFixture(t)
	body := slackBody(t, cf.mustExternal(), chatops.SlackApproveAction,
		string(sre.NewUUID()))
	if code, out := cf.slack(t, body, testSigningSecret, time.Now()); code !=
		http.StatusNotFound || out["code"] != "not_found" {
		t.Fatalf("unknown proposal = %d %v", code, out)
	}
	for _, path := range []string{"/api/v1/chatops/slack/999999",
		fmt.Sprintf("/api/v1/chatops/slack/%d", cf.tgCh), "/api/v1/chatops/slack/x"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		signSlack(req, body, testSigningSecret, time.Now())
		if code, _ := serveJSON(cf.h, req); code != http.StatusNotFound &&
			code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 404/400", path, code)
		}
	}
	other, _ := json.Marshal(map[string]any{"type": "block_actions",
		"team": map[string]any{"id": "T9999"},
		"user": map[string]any{"id": cf.mustExternal()},
		"actions": []any{map[string]any{"action_id": chatops.SlackApproveAction,
			"value": string(cf.proposal.ID), "action_ts": "1.2"}}})
	otherBody := []byte(url.Values{"payload": {string(other)}}.Encode())
	if code, out := cf.slack(t, otherBody, testSigningSecret, time.Now()); code !=
		http.StatusForbidden || out["code"] != "forbidden" {
		t.Fatalf("another workspace = %d %v, want 403", code, out)
	}
}

func telegramBody(updateID int64, user, chat string, data string) []byte {
	uid, _ := strconv.ParseInt(user, 10, 64)
	cid, _ := strconv.ParseInt(chat, 10, 64)
	raw, _ := json.Marshal(map[string]any{"update_id": updateID,
		"callback_query": map[string]any{"id": "cbq", "data": data,
			"from":    map[string]any{"id": uid, "username": "bob"},
			"message": map[string]any{"message_id": 5, "chat": map[string]any{"id": cid}}}})
	return raw
}

func (cf *chatFixture) telegram(body []byte, secret string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/telegram/%d", cf.tgCh), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	return serveJSON(cf.h, req)
}

func TestChatOpsTelegramApproval(t *testing.T) {
	cf := newChatFixture(t)
	data := chatops.CallbackData(chatops.DecisionApprove, string(cf.proposal.ID))
	update := time.Now().UnixNano()
	if code, out := cf.telegram(telegramBody(update, cf.suffix(), "-1001", data),
		"wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret = %d %v", code, out)
	}
	if code, out := cf.telegram(telegramBody(update+1, cf.suffix(), "-2002", data),
		"tg-secret_1"); code != http.StatusForbidden || out["code"] != "forbidden" {
		t.Fatalf("another chat = %d %v", code, out)
	}
	if code, out := cf.telegram(telegramBody(update+2, "424242", "-1001", data),
		"tg-secret_1"); code != http.StatusForbidden || out["code"] != "unmapped_user" {
		t.Fatalf("unmapped telegram user = %d %v", code, out)
	}
	if cf.f.canceler.count() != 0 {
		t.Fatal("a refused telegram callback executed")
	}
	if code, out := cf.telegram(telegramBody(update+3, cf.suffix(), "-1001", data),
		"tg-secret_1"); code != 200 || out["decision"] != "approve" {
		t.Fatalf("telegram approve = %d %v", code, out)
	}
	status, decided := cf.queue(t)
	if status != "approved" || decided == nil || *decided != cf.operator ||
		cf.f.canceler.count() != 1 {
		t.Fatalf("queue %s by %v, cancels %d", status, decided, cf.f.canceler.count())
	}
	if code, out := cf.telegram(telegramBody(update+3, cf.suffix(), "-1001", data),
		"tg-secret_1"); code != http.StatusConflict || out["code"] != "replay" {
		t.Fatalf("redelivered update = %d %v", code, out)
	}
	chat, _ := json.Marshal(map[string]any{"update_id": update + 4,
		"message": map[string]any{"text": "approve everything"}})
	if code, out := cf.telegram(chat, "tg-secret_1"); code != 200 || out["ignored"] != true {
		t.Fatalf("a chat message = %d %v, want ignored", code, out)
	}
}

// The callbacks skip session auth (they are signed); nothing else does.
func TestChatOpsCallbacksAreTheOnlySessionlessRoutes(t *testing.T) {
	cf := newChatFixture(t)
	mgr := fleet.NewManager(config.DefaultConfig())
	h := NewRouterFullRuntime(mgr, config.DefaultConfig(), cf.pool,
		&ActionDeps{Fleet: mgr}, nil, nil, &RuntimeDeps{}, SessionAuthMiddleware(cf.pool))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chatops/identities", nil)
	if code, _ := serveJSON(h, req); code != http.StatusUnauthorized {
		t.Fatalf("identities without a session = %d, want 401", code)
	}
	body := []byte("payload=x")
	req = httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/chatops/slack/%d", cf.slackCh), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if code, out := serveJSON(h, req); code != http.StatusUnauthorized ||
		out["code"] != "bad_signature" {
		t.Fatalf("unsigned callback = %d %v, want 401 bad_signature (not session)", code,
			out)
	}
}

func TestChatOpsIdentityAdminRoutes(t *testing.T) {
	cf := newChatFixture(t)
	mgr := fleet.NewManager(config.DefaultConfig())
	asUser := func(u *auth.User) http.Handler {
		inject := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(),
					userContextKey, u)))
			})
		}
		return NewRouterFullRuntime(mgr, config.DefaultConfig(), cf.pool,
			&ActionDeps{Fleet: mgr}, nil, nil, &RuntimeDeps{}, inject)
	}
	link := fmt.Sprintf(`{"provider":"slack","team_id":"T0001",`+
		`"external_user_id":"U-NEW-%d","user_id":%d}`, time.Now().UnixNano(), cf.operator)
	post := func(h http.Handler) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chatops/identities",
			strings.NewReader(link))
		req.Header.Set("Content-Type", "application/json")
		return serveJSON(h, req)
	}
	if code, _ := post(asUser(testOperatorUser())); code != http.StatusForbidden {
		t.Fatalf("operator linking = %d, want 403", code)
	}
	code, out := post(asUser(testAdminUser()))
	if code != http.StatusCreated || out["id"] == nil {
		t.Fatalf("admin link = %d %v", code, out)
	}
	if code, out := post(asUser(testAdminUser())); code != http.StatusConflict {
		t.Fatalf("duplicate link = %d %v, want 409", code, out)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chatops/identities", nil)
	code, list := serveJSON(asUser(testAdminUser()), req)
	items, _ := list["items"].([]any)
	if code != 200 || len(items) < 3 {
		t.Fatalf("list = %d %v", code, list)
	}
	req = httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/v1/chatops/identities/%v", out["id"]), nil)
	if code, _ := serveJSON(asUser(testAdminUser()), req); code != 200 {
		t.Fatalf("unlink = %d", code)
	}
}

func TestNotificationChannelsMaskChatOpsSecrets(t *testing.T) {
	cfg := map[string]string{"bot_token": "123456:ABCDEFGH-secret",
		"signing_secret": "8f742231b10e8888abcd", "webhook_secret": "tg-secret-value",
		"chat_id": "-1001"}
	maskChannelSecrets(cfg)
	for _, k := range []string{"bot_token", "signing_secret", "webhook_secret"} {
		if !strings.Contains(cfg[k], "****") {
			t.Errorf("%s not masked: %q", k, cfg[k])
		}
	}
	if cfg["chat_id"] != "-1001" {
		t.Fatalf("chat_id masked: %q", cfg["chat_id"])
	}
}
