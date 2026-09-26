package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// TestCreateRule_RejectsUnreachableSeverity is the G7-B06 regression:
// action_executed events are always "info", so min_severity=warning
// silently never fires.
func TestCreateRule_RejectsUnreachableSeverity(t *testing.T) {
	pool, ctx := coverageDB(t)
	ns := NewNotificationStore(pool, nil)
	chID := createRegressionChannel(t, ns, ctx, "b06")
	_, err := ns.CreateRule(ctx, chID, "action_executed", "warning")
	if !errors.Is(err, ErrValidation) ||
		!strings.Contains(err.Error(), "never") {
		t.Fatalf("err = %v, want unreachable-severity validation error", err)
	}
	id, err := ns.CreateRule(ctx, chID, "action_executed", "info")
	if err != nil || id <= 0 {
		t.Fatalf("reachable rule rejected: id=%d err=%v", id, err)
	}
}

func createRegressionChannel(
	t *testing.T, ns *NotificationStore, ctx context.Context, tag string,
) int {
	t.Helper()
	name := fmt.Sprintf("reg-%s-%d", tag, time.Now().UnixNano())
	id, err := ns.CreateChannel(ctx, name, "slack", map[string]string{
		"webhook_url": "https://hooks.slack.com/services/T/B/" + tag,
	}, 0)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() { _ = ns.DeleteChannel(context.Background(), id) })
	return id
}

// TestCreateChannel_RejectsInternalTargets is the G7-B21 regression.
func TestCreateChannel_RejectsInternalTargets(t *testing.T) {
	ns := NewNotificationStore(nil, nil)
	cases := []struct {
		typ string
		cfg map[string]string
	}{
		{"slack", map[string]string{
			"webhook_url": "http://169.254.169.254/latest/meta-data"}},
		{"slack", map[string]string{"webhook_url": "file:///etc/passwd"}},
		{"slack", map[string]string{"webhook_url": "https://127.0.0.1/x"}},
		{"email", map[string]string{"smtp_host": "10.1.2.3",
			"from": "a@example.com", "to": "b@example.com"}},
		{"email", map[string]string{"smtp_host": "localhost",
			"from": "a@example.com", "to": "b@example.com"}},
	}
	for _, tc := range cases {
		_, err := ns.CreateChannel(context.Background(), "x", tc.typ, tc.cfg, 0)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%s %v: err = %v, want ErrValidation", tc.typ, tc.cfg, err)
		}
	}
}

// TestChannelSecrets_EncryptedAtRestAndLazilyMigrated is the G7-B20
// regression: channel secrets were stored in plaintext JSONB.
func TestChannelSecrets_EncryptedAtRestAndLazilyMigrated(t *testing.T) {
	pool, ctx := coverageDB(t)
	key := bytes.Repeat([]byte{3}, 32)
	ns := NewNotificationStore(pool, nil).WithSecretKey(key)
	secretURL := "https://hooks.slack.com/services/T/B/ATREST"
	name := fmt.Sprintf("reg-b20-%d", time.Now().UnixNano())
	id, err := ns.CreateChannel(ctx, name, "slack",
		map[string]string{"webhook_url": secretURL}, 0)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() { _ = ns.DeleteChannel(context.Background(), id) })
	if raw := rawWebhook(t, ns, ctx, id); strings.Contains(raw, "ATREST") {
		t.Fatalf("stored webhook_url is plaintext: %q", raw)
	}
	ch, err := ns.GetChannel(ctx, id)
	if err != nil || ch.Config["webhook_url"] != secretURL {
		t.Fatalf("GetChannel = %v, %v", ch, err)
	}

	legacy := fmt.Sprintf("reg-b20-legacy-%d", time.Now().UnixNano())
	var legacyID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels
		(name, type, config) VALUES ($1, 'slack', $2) RETURNING id`,
		legacy, `{"webhook_url":"https://hooks.slack.com/services/LEGACY"}`,
	).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	t.Cleanup(func() { _ = ns.DeleteChannel(context.Background(), legacyID) })
	got, err := ns.GetChannel(ctx, legacyID)
	if err != nil || !strings.HasSuffix(got.Config["webhook_url"], "LEGACY") {
		t.Fatalf("legacy read = %v, %v", got, err)
	}
	if raw := rawWebhook(t, ns, ctx, legacyID); strings.Contains(raw, "LEGACY") {
		t.Fatalf("legacy row not migrated on read: %q", raw)
	}
}

func rawWebhook(
	t *testing.T, ns *NotificationStore, ctx context.Context, id int,
) string {
	t.Helper()
	var raw string
	if err := ns.pool.QueryRow(ctx, `SELECT config->>'webhook_url'
		FROM sage.notification_channels WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	return raw
}

type failingTestSender struct{}

func (failingTestSender) Type() string { return "slack" }
func (failingTestSender) Send(context.Context, notify.Channel, notify.Event) error {
	return errors.New(`Post "https://hooks.slack.com/services/T/B/LEAKME": ` +
		`dial tcp 10.0.0.7:443: i/o timeout`)
}

// TestTestChannel_LogsTestEventAndRedacts covers G7-B37 (test sends were
// logged as action_executed) and G7-B10 (URL secrets in the log).
func TestTestChannel_LogsTestEventAndRedacts(t *testing.T) {
	pool, ctx := coverageDB(t)
	d := notify.NewDispatcher(pool, func(string, string, ...any) {})
	d.RegisterSender(failingTestSender{})
	ns := NewNotificationStore(pool, d)
	chID := createRegressionChannel(t, ns, ctx, "b37")
	err := ns.TestChannel(ctx, chID)
	if err == nil {
		t.Fatal("TestChannel succeeded with failing sender")
	}
	if strings.Contains(err.Error(), "LEAKME") ||
		strings.Contains(err.Error(), "10.0.0.7") {
		t.Fatalf("TestChannel error echoes internals: %v", err)
	}
	var event, logErr string
	if err := pool.QueryRow(ctx, `SELECT event, COALESCE(error, '')
		FROM sage.notification_log WHERE channel_id = $1
		ORDER BY id DESC LIMIT 1`, chID).Scan(&event, &logErr); err != nil {
		t.Fatalf("read log: %v", err)
	}
	if event != "test" {
		t.Fatalf("logged event = %q, want test", event)
	}
	if strings.Contains(logErr, "LEAKME") {
		t.Fatalf("notification_log.error leaks webhook path: %q", logErr)
	}
}
