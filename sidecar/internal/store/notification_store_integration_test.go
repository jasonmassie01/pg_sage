//go:build integration

package store

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

func TestCreateChannel(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/test"}
	id, err := ns.CreateChannel(ctx, "test-slack", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})
}

func TestListChannels(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/list"}
	id, err := ns.CreateChannel(ctx, "list-test", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})

	channels, err := ns.ListChannels(ctx)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	found := false
	for _, ch := range channels {
		if ch.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("created channel %d not in list", id)
	}
}

func TestUpdateChannel(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/upd"}
	id, err := ns.CreateChannel(ctx, "update-test", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, id)
	})

	newCfg := map[string]string{"webhook_url": "https://hooks.slack.com/new"}
	if err := ns.UpdateChannel(
		ctx, id, "updated-name", newCfg, false,
	); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}

	ch, err := ns.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Name != "updated-name" {
		t.Errorf("Name = %q, want updated-name", ch.Name)
	}
	if ch.Enabled {
		t.Error("expected disabled after update")
	}
}

func TestDeleteChannel(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/del"}
	id, err := ns.CreateChannel(ctx, "delete-test", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	if err := ns.DeleteChannel(ctx, id); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}

	_, err = ns.GetChannel(ctx, id)
	if err == nil {
		t.Error("expected error getting deleted channel")
	}
}

func TestCreateRule(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/rule"}
	chID, err := ns.CreateChannel(ctx, "rule-ch", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	ruleID, err := ns.CreateRule(
		ctx, chID, "action_executed", "warning")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if ruleID <= 0 {
		t.Fatalf("expected positive rule ID, got %d", ruleID)
	}
}

func TestListRules(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/lr"}
	chID, err := ns.CreateChannel(ctx, "listrule-ch", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	_, err = ns.CreateRule(ctx, chID, "action_failed", "info")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}

	rules, err := ns.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) == 0 {
		t.Error("expected at least one rule")
	}
}

func TestDeleteRule(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/dr"}
	chID, err := ns.CreateChannel(ctx, "delrule-ch", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	ruleID, err := ns.CreateRule(
		ctx, chID, "approval_needed", "critical")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}

	if err := ns.DeleteRule(ctx, ruleID); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
}

func TestListLog(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	entries, err := ns.ListLog(ctx, 10)
	if err != nil {
		t.Fatalf("ListLog: %v", err)
	}
	// Just verify it runs without error; log may be empty
	_ = entries
}

func TestValidation_InvalidChannelType(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	_, err := ns.CreateChannel(
		ctx, "bad-type", "sms", map[string]string{}, 1)
	if err == nil {
		t.Error("expected validation error for invalid type")
	}
}

func TestValidation_SlackMissingWebhook(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	_, err := ns.CreateChannel(
		ctx, "no-webhook", "slack", map[string]string{}, 1)
	if err == nil {
		t.Error("expected validation error for missing webhook")
	}
}

func TestValidation_InvalidEvent(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/v"}
	chID, err := ns.CreateChannel(ctx, "val-ch", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	_, err = ns.CreateRule(ctx, chID, "bogus_event", "info")
	if err == nil {
		t.Error("expected validation error for invalid event")
	}
}

func TestValidation_InvalidSeverity(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)

	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/vs"}
	chID, err := ns.CreateChannel(ctx, "valsev-ch", "slack", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_ = ns.DeleteChannel(ctx, chID)
	})

	_, err = ns.CreateRule(
		ctx, chID, "action_executed", "extreme")
	if err == nil {
		t.Error("expected validation error for invalid severity")
	}
}

// Suppress unused import warning from notify package.
var _ = notify.ValidEventTypes
