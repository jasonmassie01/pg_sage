package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// ux-09a: notification_policy.ask_batch_minutes becomes the dispatcher's
// Ask window. No concurrent access test: askBatchWindow is pure.
func TestAskBatchWindow_FollowsTheKeyAndItsDefault(t *testing.T) {
	if got := askBatchWindow(nil); got != 15*time.Minute {
		t.Fatalf("no config: %v, want the 15 minute default", got)
	}
	c := config.DefaultConfig()
	if got := askBatchWindow(c); got != 15*time.Minute {
		t.Fatalf("default config: %v, want 15m", got)
	}
	c.NotificationPolicy.AskBatchMinutes = 0
	if got := askBatchWindow(c); got != 0 {
		t.Fatalf("ask_batch_minutes 0: %v, want 0 (each Ask alone)", got)
	}
	c.NotificationPolicy.AskBatchMinutes = 1440
	if got := askBatchWindow(c); got != 24*time.Hour {
		t.Fatalf("ask_batch_minutes 1440: %v", got)
	}
}

func TestChatApprovalCap_ReadsTheKey(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = nil
	if got := chatApprovalCap(); got != "" {
		t.Fatalf("no config: %q, want the default (empty)", got)
	}
	cfg = config.DefaultConfig()
	cfg.NotificationPolicy.ChatApprovalMaxTier = config.ChatApprovalCapSafe
	if got := chatApprovalCap(); got != "safe" {
		t.Fatalf("cap safe read as %q", got)
	}
}
