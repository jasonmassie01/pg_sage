package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestDetectPlatform_CloudSQL(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "max_wal_size", Setting: "1GB", Source: "cloud-sql"},
	}
	got := detectPlatform(settings)
	if got != "cloud-sql" {
		t.Fatalf("expected cloud-sql, got %q", got)
	}
}

func TestDetectPlatform_CloudSQLVariant(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "wal_level", Setting: "replica", Source: "cloudsql-override"},
	}
	got := detectPlatform(settings)
	if got != "cloud-sql" {
		t.Fatalf("expected cloud-sql for source containing 'cloudsql', got %q", got)
	}
}

func TestDetectPlatform_SelfManaged(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "max_wal_size", Setting: "1GB", Source: "default"},
		{Name: "wal_level", Setting: "replica", Source: "configuration file"},
	}
	got := detectPlatform(settings)
	if got != "self-managed" {
		t.Fatalf("expected self-managed, got %q", got)
	}
}

func TestDetectPlatform_EmptySettings(t *testing.T) {
	got := detectPlatform(nil)
	if got != "self-managed" {
		t.Fatalf("expected self-managed for nil settings, got %q", got)
	}

	got = detectPlatform([]collector.PGSetting{})
	if got != "self-managed" {
		t.Fatalf("expected self-managed for empty settings, got %q", got)
	}
}

func TestDetectPlatform_MixedSources(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "max_connections", Setting: "100", Source: "configuration file"},
		{Name: "shared_buffers", Setting: "128MB", Source: "default"},
		{Name: "max_wal_size", Setting: "1GB", Source: "cloud-sql"},
		{Name: "wal_level", Setting: "replica", Source: "configuration file"},
	}
	got := detectPlatform(settings)
	if got != "cloud-sql" {
		t.Fatalf("expected cloud-sql (first match wins), got %q", got)
	}
}

func TestWALSystemPrompt_ContainsRules(t *testing.T) {
	checks := []string{
		"max_wal_size",
		"JSON array",
		"ALTER SYSTEM SET",
	}
	for _, want := range checks {
		if !strings.Contains(walSystemPrompt, want) {
			t.Errorf("walSystemPrompt missing expected text %q", want)
		}
	}
}

func TestWALSystemPrompt_ContainsAntiThinking(t *testing.T) {
	if !strings.Contains(walSystemPrompt, "No thinking") {
		t.Error("walSystemPrompt missing 'No thinking' anti-thinking directive")
	}
}
