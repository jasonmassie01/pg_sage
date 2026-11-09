//go:build integration

package store

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

func smtpBindingStore(t *testing.T) (*NotificationStore, int, func() string) {
	t.Helper()
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil).WithSecretKey(bytes.Repeat([]byte{7}, 32))
	cfg := map[string]string{
		"smtp_host": "smtp.example.com", "smtp_port": "587",
		"smtp_user": "sage", "smtp_pass": "relay-secret",
		"from": "sage@example.com", "to": "dba@example.com",
	}
	name := fmt.Sprintf("smtp-binding-%d", time.Now().UnixNano())
	id, err := ns.CreateChannel(ctx, name, "email", cfg, 1)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() { _ = ns.DeleteChannel(ctx, id) })
	raw := func() string {
		var s string
		if err := pool.QueryRow(ctx,
			`SELECT config::text FROM sage.notification_channels WHERE id = $1`,
			id).Scan(&s); err != nil {
			t.Fatalf("read raw config: %v", err)
		}
		return s
	}
	return ns, id, raw
}

func smtpUpdate(host, port, pass string) map[string]string {
	return map[string]string{
		"smtp_host": host, "smtp_port": port, "smtp_user": "sage",
		"smtp_pass": pass, "from": "sage@example.com", "to": "dba@example.com",
	}
}

func TestUpdateChannelSMTPHostChangeNeedsPassword(t *testing.T) {
	ns, id, raw := smtpBindingStore(t)
	before := raw()
	for _, cfg := range []map[string]string{
		smtpUpdate("smtp.attacker.example", "587", "****"),
		smtpUpdate("smtp.example.com", "2525", "****"),
		smtpUpdate("smtp.attacker.example", "587", "rel****"),
	} {
		err := ns.UpdateChannel(t.Context(), id, "renamed", cfg, true)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("UpdateChannel(%v) = %v, want ErrValidation", cfg, err)
		}
	}
	if after := raw(); after != before {
		t.Fatalf("ciphertext changed after refused updates:\nbefore %s\nafter  %s",
			before, after)
	}
	ch, err := ns.GetChannel(t.Context(), id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Config["smtp_host"] != "smtp.example.com" || ch.Config["smtp_pass"] != "relay-secret" {
		t.Fatalf("stored config changed: host %q", ch.Config["smtp_host"])
	}
}

func TestUpdateChannelSMTPSameHostKeepsMaskedPassword(t *testing.T) {
	ns, id, _ := smtpBindingStore(t)
	if err := ns.UpdateChannel(t.Context(), id, "renamed",
		smtpUpdate("SMTP.example.com", "587", "****"), true); err != nil {
		t.Fatalf("same-host masked update: %v", err)
	}
	ch, err := ns.GetChannel(t.Context(), id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Name != "renamed" || ch.Config["smtp_pass"] != "relay-secret" {
		t.Fatalf("name %q, password kept=%v", ch.Name, ch.Config["smtp_pass"] == "relay-secret")
	}
}

func TestUpdateChannelSMTPHostChangeWithNewPassword(t *testing.T) {
	ns, id, _ := smtpBindingStore(t)
	if err := ns.UpdateChannel(t.Context(), id, "moved",
		smtpUpdate("smtp2.example.com", "465", "new-secret"), true); err != nil {
		t.Fatalf("host change with password: %v", err)
	}
	ch, err := ns.GetChannel(t.Context(), id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Config["smtp_host"] != "smtp2.example.com" || ch.Config["smtp_pass"] != "new-secret" {
		t.Fatalf("host %q, new password stored=%v", ch.Config["smtp_host"],
			ch.Config["smtp_pass"] == "new-secret")
	}
}
