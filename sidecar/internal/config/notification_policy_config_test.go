package config

import (
	"os"
	"path/filepath"
	"testing"
)

// G7-B21: private notification targets are refused unless an operator opts
// in. The default must be false even with no config file present.
func TestNotificationPolicyAllowPrivateTargetsDefaultsFalse(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--pg-url", "postgres://u:p@db.example:5432/app"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NotificationPolicy.AllowPrivateTargets {
		t.Fatal("allow_private_targets defaults to true")
	}
	if DefaultConfig().NotificationPolicy.AllowPrivateTargets {
		t.Fatal("DefaultConfig allows private targets")
	}
}

func TestNotificationPolicyAllowPrivateTargetsLoadsFromYAML(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "mode: standalone\n" +
		"postgres:\n  host: db.example\n  database: app\n  user: u\n" +
		"notification_policy:\n  allow_private_targets: true\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.NotificationPolicy.AllowPrivateTargets {
		t.Fatal("notification_policy.allow_private_targets: true was not loaded")
	}
	if clone := Clone(cfg); !clone.NotificationPolicy.AllowPrivateTargets {
		t.Fatal("Clone dropped notification_policy.allow_private_targets")
	}
}

// The target policy is baked into senders at startup and is a security
// boundary, so it must never be treated as live-reloadable.
func TestNotificationPolicyAllowPrivateTargetsRequiresRestart(t *testing.T) {
	lifecycle, ok := LookupFieldLifecycle("notification_policy.allow_private_targets")
	if !ok {
		t.Fatal("notification_policy.allow_private_targets has no lifecycle entry")
	}
	if lifecycle.Lifecycle != LifecycleRestart {
		t.Fatalf("lifecycle = %q, want restart", lifecycle.Lifecycle)
	}
}
