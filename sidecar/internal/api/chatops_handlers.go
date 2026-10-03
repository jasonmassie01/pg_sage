package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// ChatOps approval callbacks (AI-SRE-SPEC R1.1). Slack and Telegram post
// button presses here. The routes sit outside session authentication:
// each request is authenticated by its provider signature instead (Slack
// v0 HMAC within the timestamp tolerance; Telegram's per-bot secret
// token), processed once, and attributed to the pg_sage user the chat
// user is explicitly mapped to. The decision goes through the existing
// approval path, so single use and decided_by hold.

// chatopsRun runs the chat reply after the callback is answered.
var chatopsRun = func(fn func()) { go fn() }

// chatopsReply answers a decision in its chat.
var chatopsReply = replyInChat

const (
	chatopsMaxBody      = 64 << 10
	chatopsReplyTimeout = 15 * time.Second
)

var numericChatID = regexp.MustCompile(`^-?[0-9]+$`)

type chatopsDeps struct {
	mgr       *fleet.DatabaseManager
	channels  *store.NotificationStore
	ids       *chatops.Store
	cards     *chatops.CardStore
	tolerance time.Duration
}

// registerChatOpsRoutes mounts the signed callbacks on the root mux and
// the admin identity mapping routes on the session-authenticated API mux.
func registerChatOpsRoutes(root, apiMux *http.ServeMux, pool *pgxpool.Pool,
	mgr *fleet.DatabaseManager, cfg *config.Config, rt RuntimeDeps) {
	d := &chatopsDeps{mgr: mgr, ids: chatops.NewStore(pool), cards: chatops.NewCardStore(pool),
		channels:  store.NewNotificationStore(pool, nil).WithSecretKey(rt.NotificationSecretKey),
		tolerance: 5 * time.Minute}
	if cfg != nil {
		d.tolerance = cfg.SRE.Actions.ChatOpsTolerance()
	}
	wrap := func(h http.HandlerFunc) http.Handler {
		return securityHeadersMiddleware(timeoutMiddleware(h))
	}
	root.Handle("POST /api/v1/chatops/slack/{channel}",
		wrap(d.callbackHandler(chatops.ProviderSlack)))
	root.Handle("POST /api/v1/chatops/telegram/{channel}",
		wrap(d.callbackHandler(chatops.ProviderTelegram)))
	registerChatOpsIdentityRoutes(apiMux, d.ids)
}

func (d *chatopsDeps) callbackHandler(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, ok := d.channel(w, r, provider)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, chatopsMaxBody))
		if err != nil {
			sreErrorCode(w, "callback body too large or unreadable", "invalid_request",
				http.StatusBadRequest)
			return
		}
		a, ok := d.verifyAndParse(w, r, ch, body)
		if !ok {
			return
		}
		user, ok := d.authorize(w, r, ch, a)
		if !ok {
			return
		}
		if a.CardToken != "" {
			d.decideCard(w, r, ch, a, user)
			return
		}
		d.decide(w, r, ch, a, user)
	}
}

// channel loads the enabled channel of the provider named by the path.
func (d *chatopsDeps) channel(w http.ResponseWriter, r *http.Request,
	provider string) (notify.Channel, bool) {
	id, err := strconv.Atoi(r.PathValue("channel"))
	if err != nil || id <= 0 {
		sreErrorCode(w, "invalid channel id", "invalid_request", http.StatusBadRequest)
		return notify.Channel{}, false
	}
	ch, err := d.channels.GetChannel(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		internalError(w, r, "chatops channel", err)
		return notify.Channel{}, false
	case ch.Type == provider && ch.Enabled:
		return *ch, true
	}
	sreErrorCode(w, "channel not found", "not_found", http.StatusNotFound)
	return notify.Channel{}, false
}

// verifyAndParse authenticates the delivery and reads its decision.
func (d *chatopsDeps) verifyAndParse(w http.ResponseWriter, r *http.Request,
	ch notify.Channel, body []byte) (chatops.Action, bool) {
	var a chatops.Action
	var err error
	if ch.Type == chatops.ProviderSlack {
		err = chatops.VerifySlack(ch.Config["signing_secret"], r.Header, body, time.Now(),
			d.tolerance)
	} else {
		err = chatops.VerifyTelegram(ch.Config["webhook_secret"], r.Header)
	}
	if err != nil {
		slog.Warn("chatops callback refused", "channel", ch.ID, "error", err)
		sreErrorCode(w, "callback signature refused", "bad_signature",
			http.StatusUnauthorized)
		return a, false
	}
	if ch.Type == chatops.ProviderSlack {
		a, err = chatops.ParseSlack(body)
	} else {
		a, err = chatops.ParseTelegram(body)
	}
	switch {
	case errors.Is(err, chatops.ErrNotCallback):
		jsonResponse(w, map[string]any{"ok": true, "ignored": true})
		return a, false
	case err != nil:
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
		return a, false
	}
	return a, true
}

// authorize scopes the decision to the channel's workspace or chat,
// refuses replays and resolves the mapped operator.
func (d *chatopsDeps) authorize(w http.ResponseWriter, r *http.Request,
	ch notify.Channel, a chatops.Action) (auth.User, bool) {
	if !chatScopeMatches(ch, a) {
		sreErrorCode(w, "the decision came from another workspace or chat", "forbidden",
			http.StatusForbidden)
		return auth.User{}, false
	}
	if err := d.ids.MarkSeen(r.Context(), a.Provider, a.Nonce); err != nil {
		if errors.Is(err, chatops.ErrReplay) {
			sreErrorCode(w, "callback already processed", "replay", http.StatusConflict)
		} else {
			internalError(w, r, "chatops replay", err)
		}
		return auth.User{}, false
	}
	user, err := d.ids.Resolve(r.Context(), a.Provider, a.TeamID, a.UserID)
	switch {
	case errors.Is(err, chatops.ErrUnmapped):
		sreErrorCode(w, "this chat user is not mapped to a pg_sage user",
			"unmapped_user", http.StatusForbidden)
		return auth.User{}, false
	case err != nil:
		internalError(w, r, "chatops identity", err)
		return auth.User{}, false
	case user.Role != auth.RoleAdmin && user.Role != auth.RoleOperator:
		sreErrorCode(w, "approving needs an operator or admin", "forbidden",
			http.StatusForbidden)
		return auth.User{}, false
	}
	return user, true
}

// chatScopeMatches: a Slack channel with a team_id accepts only its
// workspace; a Telegram channel accepts only its own (numeric) chat.
func chatScopeMatches(ch notify.Channel, a chatops.Action) bool {
	if ch.Type == chatops.ProviderSlack {
		team := ch.Config["team_id"]
		return team == "" || team == a.TeamID
	}
	chat := ch.Config["chat_id"]
	return numericChatID.MatchString(chat) && chat == a.ChatID
}

// reply answers the decision in its chat once the callback is answered.
func reply(ch notify.Channel, a chatops.Action, text string) {
	chatopsRun(func() {
		ctx, cancel := context.WithTimeout(context.Background(), chatopsReplyTimeout)
		defer cancel()
		if err := chatopsReply(ctx, ch, a, text); err != nil {
			slog.Warn("chatops reply failed", "channel", ch.ID, "error", err)
		}
	})
}

// replyInChat answers through the provider's sender with the strict
// target policy: replies only ever go to Slack's and Telegram's hosts.
func replyInChat(ctx context.Context, ch notify.Channel, a chatops.Action,
	text string) error {
	switch ch.Type {
	case chatops.ProviderSlack:
		return notify.NewSlackSender().Reply(ctx, ch, a, text)
	case chatops.ProviderTelegram:
		return notify.NewTelegramSender().Reply(ctx, ch, a, text)
	}
	return fmt.Errorf("chatops: channel type %q cannot reply", ch.Type)
}
