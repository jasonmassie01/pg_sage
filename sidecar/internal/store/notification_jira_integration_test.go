//go:build integration

package store

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// Integration (market-14): a Jira integration is saved like a channel,
// with its api_token sealed, but no rule can route events to it and its
// test button explains how it is checked instead of sending.

func TestJiraIntegration_RuleRefusedTokenSealedTestExplained(t *testing.T) {
	pool, ctx := requireDB(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ns := NewNotificationStore(pool, notify.NewDefaultDispatcher(nil, notify.TargetPolicy{},
		nil)).WithSecretKey(key)
	id, err := ns.CreateChannel(ctx, "m14-jira", "jira", jiraConfig(), 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.notification_channels WHERE id = $1", id)
	})
	var stored string
	if err := pool.QueryRow(ctx, `SELECT config->>'api_token'
		FROM sage.notification_channels WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "t" || stored == "" {
		t.Fatalf("api_token stored as %q, want it sealed", stored)
	}
	if _, err := ns.CreateRule(ctx, id, "finding_critical", "critical"); !errors.Is(err,
		ErrValidation) {
		t.Fatalf("rule on a jira channel: err = %v, want ErrValidation", err)
	}
	var rules int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.notification_rules
		WHERE channel_id = $1`, id).Scan(&rules); err != nil || rules != 0 {
		t.Fatalf("rules on the jira channel = %d (%v), want 0", rules, err)
	}
	if err := ns.TestChannel(ctx, id); !errors.Is(err, ErrValidation) {
		t.Fatalf("TestChannel(jira) = %v, want ErrValidation", err)
	}
	ch, err := ns.GetChannel(ctx, id)
	if err != nil || ch.Config["api_token"] != "t" {
		t.Fatalf("GetChannel = %+v, %v; want the token opened", ch, err)
	}
}

// Moving the integration to another site while sending the masked token
// back is refused, so the stored token never goes to the new site.
func TestJiraIntegration_MovingSiteNeedsTheTokenAgain(t *testing.T) {
	pool, ctx := requireDB(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	ns := NewNotificationStore(pool, nil).WithSecretKey(key)
	cfg := jiraConfig()
	cfg["api_token"] = "stored-api-token"
	id, err := ns.CreateChannel(ctx, "m14-jira-move", "jira", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.notification_channels WHERE id = $1", id)
	})
	moved := jiraConfig()
	moved["base_url"], moved["api_token"] = "https://evil.example", "stor****oken"
	err = ns.UpdateChannel(ctx, id, "m14-jira-move", moved, true)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateChannel(moved, masked token) = %v, want ErrValidation", err)
	}
	ch, err := ns.GetChannel(ctx, id)
	if err != nil || ch.Config["base_url"] != "https://acme.atlassian.net" ||
		ch.Config["api_token"] != "stored-api-token" {
		t.Fatalf("after refusal: %+v, %v; want the stored site and token kept", ch, err)
	}
	same := jiraConfig()
	same["api_token"], same["project_key"] = "stor****oken", "DBA"
	if err := ns.UpdateChannel(ctx, id, "m14-jira-move", same, true); err != nil {
		t.Fatalf("UpdateChannel(same site, masked token) = %v, want nil", err)
	}
	ch, err = ns.GetChannel(ctx, id)
	if err != nil || ch.Config["project_key"] != "DBA" ||
		ch.Config["api_token"] != "stored-api-token" {
		t.Fatalf("after same-site update: %+v, %v; want DBA and the token kept", ch, err)
	}
}
