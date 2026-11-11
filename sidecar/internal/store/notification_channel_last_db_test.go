//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alerting-10: GET /api/v1/notifications/channels shows each channel's
// last delivery attempt, read from the newest notification_log row of
// that channel. The Channels UI edits a channel by sending its masked
// secrets back; the store must keep the originals.
// No concurrent access tests: the list is one read-only query and an
// update is a single-row UPDATE; there is no shared in-process state.

func lastDeliveryChannel(
	t *testing.T, ns *NotificationStore, ctx context.Context, name string,
) int {
	t.Helper()
	id, err := ns.CreateChannel(ctx, name, "slack",
		map[string]string{"webhook_url": "https://hooks.slack.com/services/T/B/" + name}, 1)
	if err != nil {
		t.Fatalf("CreateChannel %s: %v", name, err)
	}
	t.Cleanup(func() { _ = ns.DeleteChannel(context.Background(), id) })
	return id
}

func insertLogRow(
	t *testing.T, pool *pgxpool.Pool, channelID *int,
	status, errMsg string, sentAt time.Time,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sage.notification_log
		    (channel_id, event, subject, body, status, error, sent_at)
		 VALUES ($1, 'test', 'alerting-10', '', $2, NULLIF($3, ''), $4)`,
		channelID, status, errMsg, sentAt)
	if err != nil {
		t.Fatalf("inserting notification_log row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_log WHERE subject = 'alerting-10'`)
	})
}

func listingByID(t *testing.T, ns *NotificationStore, ctx context.Context) map[int]ChannelListing {
	t.Helper()
	list, err := ns.ListChannels(ctx)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	out := make(map[int]ChannelListing, len(list))
	for _, ch := range list {
		out[ch.ID] = ch
	}
	return out
}

func TestListChannels_LastDeliveryIsTheNewestLogRow(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	a := lastDeliveryChannel(t, ns, ctx, "a10-newest")
	b := lastDeliveryChannel(t, ns, ctx, "a10-failed")

	base := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	// Channel a: an old failure, then a newer success. The success wins.
	insertLogRow(t, pool, &a, "error", "connection refused", base)
	insertLogRow(t, pool, &a, "sent", "", base.Add(time.Hour))
	// Channel b: an old success, then a failure that embeds a webhook URL.
	insertLogRow(t, pool, &b, "sent", "", base)
	insertLogRow(t, pool, &b, "error",
		"post https://hooks.slack.com/services/T0/B0/XYZ: 404", base.Add(2*time.Hour))
	// A row with no channel never shows on a channel.
	insertLogRow(t, pool, nil, "error", "orphan", base.Add(3*time.Hour))

	got := listingByID(t, ns, ctx)
	ca, ok := got[a]
	if !ok {
		t.Fatalf("channel %d missing from the list", a)
	}
	if ca.LastStatus != "sent" || ca.LastError != "" {
		t.Errorf("channel a last = %q/%q, want sent with no error", ca.LastStatus, ca.LastError)
	}
	if ca.LastSentAt == nil || !ca.LastSentAt.Equal(base.Add(time.Hour)) {
		t.Errorf("channel a last_sent_at = %v, want %v", ca.LastSentAt, base.Add(time.Hour))
	}
	cb := got[b]
	if cb.LastStatus != "error" {
		t.Errorf("channel b last_status = %q, want error", cb.LastStatus)
	}
	if strings.Contains(cb.LastError, "XYZ") || !strings.Contains(cb.LastError, "404") {
		t.Errorf("channel b last_error = %q, want the URL redacted and the cause kept",
			cb.LastError)
	}
	if cb.LastSentAt == nil || !cb.LastSentAt.Equal(base.Add(2*time.Hour)) {
		t.Errorf("channel b last_sent_at = %v, want %v", cb.LastSentAt, base.Add(2*time.Hour))
	}
	if ca.Config["webhook_url"] == "" || ca.Name != "a10-newest" || ca.Type != "slack" {
		t.Errorf("channel a fields lost: %+v", ca.Channel)
	}
}

func TestListChannels_SameTimestampBreaksTieByNewestRow(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	id := lastDeliveryChannel(t, ns, ctx, "a10-tie")
	at := time.Date(2030, 2, 3, 4, 5, 6, 0, time.UTC)
	insertLogRow(t, pool, &id, "error", "first", at)
	insertLogRow(t, pool, &id, "sent", "", at)

	got := listingByID(t, ns, ctx)[id]
	if got.LastStatus != "sent" {
		t.Errorf("last_status = %q, want the later-inserted sent row", got.LastStatus)
	}
}

func TestListChannels_NoLogRowsMeansNeverDelivered(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	id := lastDeliveryChannel(t, ns, ctx, "a10-never")

	got, ok := listingByID(t, ns, ctx)[id]
	if !ok {
		t.Fatalf("channel %d missing from the list", id)
	}
	if got.LastStatus != "" || got.LastError != "" || got.LastSentAt != nil {
		t.Errorf("a channel with no log rows has last %q/%q/%v, want empty",
			got.LastStatus, got.LastError, got.LastSentAt)
	}
	if !got.Enabled {
		t.Errorf("a new channel should be enabled")
	}
}

func TestUpdateChannel_MaskedSecretsKeepTheOriginals(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil).WithSecretKey(testKey)
	cfg := map[string]string{
		"webhook_url":    "https://hooks.slack.com/services/T1/B1/orig",
		"interactive":    "true",
		"signing_secret": "orig-signing-secret-value",
		"team_id":        "T0001",
	}
	id, err := ns.CreateChannel(ctx, "a10-masked", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() { _ = ns.DeleteChannel(context.Background(), id) })

	// What the UI sends back after renaming: the masked values it was shown.
	edited := map[string]string{
		"webhook_url":    "http****orig",
		"interactive":    "true",
		"signing_secret": "****",
		"team_id":        "T0002",
	}
	if err := ns.UpdateChannel(ctx, id, "a10-renamed", edited, true); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	ch, err := ns.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Name != "a10-renamed" || ch.Config["team_id"] != "T0002" {
		t.Errorf("edit not applied: name %q team_id %q", ch.Name, ch.Config["team_id"])
	}
	if ch.Config["signing_secret"] != cfg["signing_secret"] {
		t.Errorf("signing_secret = %q, want the original kept", ch.Config["signing_secret"])
	}
	if ch.Config["webhook_url"] != cfg["webhook_url"] {
		t.Errorf("webhook_url = %q, want the original kept", ch.Config["webhook_url"])
	}
}

func TestUpdateChannel_TelegramMaskedSecretsKeepTheOriginals(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil).WithSecretKey(testKey)
	cfg := map[string]string{
		"bot_token":      "123456:ABCdef_ghi-JKL",
		"chat_id":        "-1001234567890",
		"webhook_secret": "abcdefghijklmnopqrstuvwxyz012345",
	}
	id, err := ns.CreateChannel(ctx, "a10-telegram", "telegram", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel telegram: %v", err)
	}
	t.Cleanup(func() { _ = ns.DeleteChannel(context.Background(), id) })

	edited := map[string]string{
		"bot_token": "1234****-JKL", "chat_id": "-1009", "webhook_secret": "abcd****2345",
	}
	if err := ns.UpdateChannel(ctx, id, "a10-telegram", edited, false); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	ch, err := ns.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Config["bot_token"] != cfg["bot_token"] ||
		ch.Config["webhook_secret"] != cfg["webhook_secret"] {
		t.Errorf("telegram secrets not kept: %v", ch.Config)
	}
	if ch.Config["chat_id"] != "-1009" || ch.Enabled {
		t.Errorf("edit not applied: chat_id %q enabled %v", ch.Config["chat_id"], ch.Enabled)
	}
}

func TestCreateChannel_ChatOpsInvalidConfigIsAValidationError(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	cases := []struct {
		name string
		typ  string
		cfg  map[string]string
		want string
	}{
		{"telegram chat_id not numeric", "telegram",
			map[string]string{"bot_token": "1:abc", "chat_id": "abc"}, "chat_id"},
		{"interactive slack without signing secret", "slack",
			map[string]string{"webhook_url": "https://hooks.slack.com/x", "interactive": "true"},
			"signing_secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := ns.CreateChannel(ctx, "a10-invalid", tc.typ, tc.cfg, 1)
			if err == nil {
				_ = ns.DeleteChannel(context.Background(), id)
				t.Fatalf("CreateChannel accepted %v", tc.cfg)
			}
			if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a validation error naming %s", err, tc.want)
			}
		})
	}
}
