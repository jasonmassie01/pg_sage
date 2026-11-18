package config

import (
	"strings"
	"testing"
)

// ux-09a, decision D7: notification_policy.chat_approval_max_tier caps the
// risk tier a chat card may approve, and ask_batch_minutes is the Ask
// batching window. Both are YAML-only and restart-bound. No concurrent
// access tests: Load builds a fresh Config per call and shares nothing.

func TestNotificationPolicyClassKeysDefaultWithoutAConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--pg-url", "postgres://u:p@db.example:5432/app"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	np := cfg.NotificationPolicy
	if np.ChatApprovalMaxTier != ChatApprovalCapModerate || np.AskBatchMinutes != 15 {
		t.Fatalf("defaults = %q / %d, want moderate / 15", np.ChatApprovalMaxTier,
			np.AskBatchMinutes)
	}
	if d := DefaultConfig().NotificationPolicy; d.ChatApprovalMaxTier != "moderate" ||
		d.AskBatchMinutes != 15 {
		t.Fatalf("DefaultConfig = %+v", d)
	}
}

func TestNotificationPolicyChatCapLoadsEveryAllowedValue(t *testing.T) {
	for _, value := range []string{"safe", "moderate", "none"} {
		cfg, err := loadYAMLResult(t, standaloneIOYAML+
			"notification_policy:\n  chat_approval_max_tier: "+value+"\n")
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if cfg.NotificationPolicy.ChatApprovalMaxTier != value {
			t.Fatalf("%s loaded as %q", value, cfg.NotificationPolicy.ChatApprovalMaxTier)
		}
	}
}

// The key can never name high: high is approved in the app only.
func TestNotificationPolicyChatCapRefusesHighAndUnknownValues(t *testing.T) {
	for _, value := range []string{"high", "HIGH", "all", "'  '"} {
		_, err := loadYAMLResult(t, standaloneIOYAML+
			"notification_policy:\n  chat_approval_max_tier: "+value+"\n")
		if err == nil {
			t.Fatalf("chat_approval_max_tier %s was accepted", value)
		}
		for _, want := range []string{"notification_policy.chat_approval_max_tier",
			"safe", "moderate", "none"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: error %q lacks %q", value, err, want)
			}
		}
	}
}

func TestNotificationPolicyAskBatchMinutesBounds(t *testing.T) {
	for value, ok := range map[string]bool{"0": true, "1": true, "15": true, "1440": true,
		"-1": false, "1441": false} {
		cfg, err := loadYAMLResult(t, standaloneIOYAML+
			"notification_policy:\n  ask_batch_minutes: "+value+"\n")
		if ok != (err == nil) {
			t.Fatalf("ask_batch_minutes %s: err %v, want ok=%v", value, err, ok)
		}
		if !ok && !strings.Contains(err.Error(), "notification_policy.ask_batch_minutes") {
			t.Fatalf("ask_batch_minutes %s: error %q does not name the key", value, err)
		}
		if ok && value == "0" && cfg.NotificationPolicy.AskBatchMinutes != 0 {
			t.Fatalf("an explicit 0 became %d (default masking)",
				cfg.NotificationPolicy.AskBatchMinutes)
		}
	}
}

// The cap is a safety boundary: it cannot be raised through the API or a
// stored override, only by editing the YAML and restarting.
func TestNotificationPolicyClassKeysAreRestartBoundAndNotOverridable(t *testing.T) {
	for _, key := range []string{"notification_policy.chat_approval_max_tier",
		"notification_policy.ask_batch_minutes"} {
		lifecycle, ok := LookupFieldLifecycle(key)
		if !ok || lifecycle.Lifecycle != LifecycleRestart {
			t.Fatalf("%s lifecycle = %+v (found %v), want restart", key, lifecycle, ok)
		}
		if spec, ok := LookupKey(key); ok && spec.Overridable {
			t.Fatalf("%s is an API override", key)
		}
	}
}
