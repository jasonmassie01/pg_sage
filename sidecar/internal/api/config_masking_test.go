package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-03: a persisted UI override that replaces an explicit file-layer
// value (YAML, env, flag) is reported as masked; for trust.level and the
// trust.tier3_* switches the most restrictive of the two applies.
//
// No concurrent-access tests here: replay and the PUT re-mask work on a
// candidate the caller owns; the controller's notice log has its own
// concurrency test in internal/config.

// isolateMaskingEnv clears the developer's SAGE_* variables that would
// change the file layer.
func isolateMaskingEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"SAGE_TRUST_LEVEL", "SAGE_LLM_API_KEY",
		"SAGE_LLM_MODEL", "SAGE_CONFIG_PATH", "SAGE_MODE", "SAGE_META_DB"} {
		t.Setenv(name, "")
	}
	t.Chdir(t.TempDir())
	t.Setenv("SAGE_DATABASE_URL", "postgres://sage@localhost:5432/app?sslmode=disable")
}

// loadMaskingYAML is the file layer: Load of a YAML file.
func loadMaskingYAML(t *testing.T, yaml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("load %q: %v", yaml, err)
	}
	return cfg
}

func loadMaskingBase(t *testing.T, yaml string) *config.Config {
	t.Helper()
	isolateMaskingEnv(t)
	return loadMaskingYAML(t, yaml)
}

var maskingAt = time.Date(2026, 11, 9, 22, 15, 0, 0, time.UTC)

func storedOverride(key, value string) store.ConfigOverride {
	return store.ConfigOverride{Key: key, Value: value, UpdatedBy: 42,
		UpdatedAt: maskingAt}
}

func replayMasked(
	t *testing.T, candidate *config.Config, overrides ...store.ConfigOverride,
) []config.MaskedKey {
	t.Helper()
	var warnings []string
	masked := ReplayStoredOverrides(candidate, overrides, func(msg string) {
		warnings = append(warnings, msg)
	})
	if len(warnings) != 0 {
		t.Fatalf("unexpected replay warnings: %q", warnings)
	}
	return masked
}

func onlyMasked(t *testing.T, masked []config.MaskedKey, key string) config.MaskedKey {
	t.Helper()
	if len(masked) != 1 || masked[0].Key != key {
		t.Fatalf("masked = %+v, want exactly %s", masked, key)
	}
	return masked[0]
}

func TestReplayTrustLevelExplicitInYAMLTakesTheRestrictive(t *testing.T) {
	candidate := loadMaskingBase(t, "trust:\n  level: observation\n")
	masked := replayMasked(t, candidate, storedOverride("trust.level", "autonomous"))
	if candidate.Trust.Level != "observation" {
		t.Fatalf("effective trust.level = %q, want observation (file is stricter)",
			candidate.Trust.Level)
	}
	got := onlyMasked(t, masked, "trust.level")
	if got.FileValue != "observation" || got.OverrideValue != "autonomous" ||
		got.EffectiveValue != "observation" || got.UpdatedBy != 42 ||
		!got.UpdatedAt.Equal(maskingAt) {
		t.Fatalf("masked entry = %+v", got)
	}
	if list := candidate.MaskedByOverride(); len(list) != 1 || list[0] != got {
		t.Fatalf("candidate carries %+v, want the returned list", list)
	}
}

func TestReplayOverrideEqualToFileValueIsNotMasked(t *testing.T) {
	candidate := loadMaskingBase(t, "trust:\n  level: advisory\n")
	masked := replayMasked(t, candidate, storedOverride("trust.level", "advisory"))
	if len(masked) != 0 {
		t.Fatalf("an override equal to the file value was reported: %+v", masked)
	}
	if candidate.Trust.Level != "advisory" {
		t.Fatalf("trust.level = %q, want advisory", candidate.Trust.Level)
	}
}

func TestReplayExplicitDefaultValueStillMasks(t *testing.T) {
	candidate := loadMaskingBase(t, "trust:\n  level: observation\n")
	if candidate.Trust.Level != config.DefaultTrustLevel {
		t.Fatalf("fixture: observation must be the default, got %q",
			config.DefaultTrustLevel)
	}
	masked := replayMasked(t, candidate, storedOverride("trust.level", "advisory"))
	onlyMasked(t, masked, "trust.level")
	if candidate.Trust.Level != "observation" {
		t.Fatalf("trust.level = %q, want the explicit observation", candidate.Trust.Level)
	}
}

func TestReplayDefaultOnlyValueNeverMasks(t *testing.T) {
	candidate := loadMaskingBase(t, "collector:\n  interval_seconds: 30\n")
	masked := replayMasked(t, candidate,
		storedOverride("trust.level", "autonomous"),
		storedOverride("collector.batch_size", "200"))
	if len(masked) != 0 {
		t.Fatalf("default-only values were reported masked: %+v", masked)
	}
	if candidate.Trust.Level != "autonomous" || candidate.Collector.BatchSize != 200 {
		t.Fatalf("overrides did not win over defaults: trust %q batch %d",
			candidate.Trust.Level, candidate.Collector.BatchSize)
	}
}

func TestReplayTier3FileFalseOverrideTrueStaysFalse(t *testing.T) {
	candidate := loadMaskingBase(t, "trust:\n  tier3_moderate: false\n")
	masked := replayMasked(t, candidate, storedOverride("trust.tier3_moderate", "true"))
	if candidate.Trust.Tier3Moderate {
		t.Fatal("tier3_moderate = true, want false (boolean AND of file and override)")
	}
	got := onlyMasked(t, masked, "trust.tier3_moderate")
	if got.FileValue != "false" || got.OverrideValue != "true" || got.EffectiveValue != "false" {
		t.Fatalf("masked entry = %+v", got)
	}
}

func TestReplayNonTrustKeyOverrideWinsAndIsMasked(t *testing.T) {
	candidate := loadMaskingBase(t, "collector:\n  batch_size: 100\n")
	masked := replayMasked(t, candidate, storedOverride("collector.batch_size", "200"))
	if candidate.Collector.BatchSize != 200 {
		t.Fatalf("batch_size = %d, want the override 200", candidate.Collector.BatchSize)
	}
	got := onlyMasked(t, masked, "collector.batch_size")
	if got.FileValue != "100" || got.OverrideValue != "200" || got.EffectiveValue != "200" {
		t.Fatalf("masked entry = %+v", got)
	}
}

func TestReplayEnvTrustLevelIsExplicitLikeYAML(t *testing.T) {
	isolateMaskingEnv(t)
	t.Setenv("SAGE_TRUST_LEVEL", "observation")
	candidate := loadMaskingYAML(t, "collector:\n  interval_seconds: 30\n")
	masked := replayMasked(t, candidate, storedOverride("trust.level", "autonomous"))
	if candidate.Trust.Level != "observation" {
		t.Fatalf("trust.level = %q, want SAGE_TRUST_LEVEL's observation",
			candidate.Trust.Level)
	}
	onlyMasked(t, masked, "trust.level")
}

func TestReplayMaskedSecretNeverShowsEitherValue(t *testing.T) {
	candidate := loadMaskingBase(t, "llm:\n  api_key: file-secret-0001\n")
	masked := replayMasked(t, candidate, storedOverride("llm.api_key", "ui-secret-0002"))
	got := onlyMasked(t, masked, "llm.api_key")
	for _, field := range []string{got.FileValue, got.OverrideValue, got.EffectiveValue} {
		if strings.Contains(field, "file-secret") || strings.Contains(field, "ui-secret") {
			t.Fatalf("masked entry leaks a secret: %+v", got)
		}
	}
	if candidate.LLM.APIKey != "ui-secret-0002" {
		t.Fatal("the override did not win for llm.api_key")
	}
}

func TestReplayWithoutOverridesReportsNothing(t *testing.T) {
	candidate := loadMaskingBase(t, "trust:\n  level: observation\n")
	if masked := replayMasked(t, candidate); len(masked) != 0 {
		t.Fatalf("masked = %+v with no stored override", masked)
	}
	if candidate.MaskedByOverride() != nil {
		t.Fatal("candidate carries a masked list without overrides")
	}
	if candidate.OverrideFileBase() == nil {
		t.Fatal("replay did not record the file base a later PUT re-masks against")
	}
}

func TestReplayInvalidStoredRowIsSkippedNotMasked(t *testing.T) {
	candidate := loadMaskingBase(t, "collector:\n  batch_size: 100\n")
	var warnings []string
	masked := ReplayStoredOverrides(candidate,
		[]store.ConfigOverride{storedOverride("collector.batch_size", "abc")},
		func(msg string) { warnings = append(warnings, msg) })
	if len(masked) != 0 || candidate.Collector.BatchSize != 100 {
		t.Fatalf("invalid row: masked %+v batch %d, want none and 100",
			masked, candidate.Collector.BatchSize)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "collector.batch_size") {
		t.Fatalf("warnings = %q, want one naming the key", warnings)
	}
}

// The PUT path applies onto the desired config: the restrictive rule and
// the masked list must follow the write, not wait for the next reload.
func TestMaskedWritesTrustRaiseOverExplicitFileIsHeldBack(t *testing.T) {
	candidate := loadMaskingBase(t, "trust:\n  level: advisory\n"+
		"collector:\n  batch_size: 100\n")
	replayMasked(t, candidate, storedOverride("collector.batch_size", "200"))
	writes := []store.ConfigOverrideWrite{{Key: "trust.level", Value: "autonomous"}}
	for _, write := range writes {
		hotReload(candidate, write.Key, write.Value)
	}
	applyMaskedWrites(candidate, writes, 9, maskingAt)
	if candidate.Trust.Level != "advisory" {
		t.Fatalf("PUT raised trust.level to %q over the explicit advisory",
			candidate.Trust.Level)
	}
	masked := candidate.MaskedByOverride()
	if len(masked) != 2 || masked[0].Key != "collector.batch_size" ||
		masked[1].Key != "trust.level" || masked[1].UpdatedBy != 9 {
		t.Fatalf("masked after PUT = %+v, want batch_size (kept) and trust.level", masked)
	}
}

func TestMaskedWritesValueEqualToFileClearsTheEntry(t *testing.T) {
	candidate := loadMaskingBase(t, "collector:\n  batch_size: 100\n")
	replayMasked(t, candidate, storedOverride("collector.batch_size", "200"))
	writes := []store.ConfigOverrideWrite{{Key: "collector.batch_size", Value: "100"}}
	hotReload(candidate, "collector.batch_size", "100")
	applyMaskedWrites(candidate, writes, 9, maskingAt)
	if got := candidate.MaskedByOverride(); len(got) != 0 {
		t.Fatalf("masked = %+v after writing the file value", got)
	}
}

func TestMaskedWritesWithoutFileBaseKeepOverridesWinning(t *testing.T) {
	candidate := config.DefaultConfig() // never loaded: no file layer known
	writes := []store.ConfigOverrideWrite{{Key: "trust.level", Value: "autonomous"}}
	hotReload(candidate, "trust.level", "autonomous")
	applyMaskedWrites(candidate, writes, 1, maskingAt)
	if candidate.Trust.Level != "autonomous" || candidate.MaskedByOverride() != nil {
		t.Fatalf("no file layer: trust %q masked %+v, want autonomous and none",
			candidate.Trust.Level, candidate.MaskedByOverride())
	}
}

func TestMarkMaskedEntriesShowsTheEffectiveValue(t *testing.T) {
	candidate := loadMaskingBase(t, "trust:\n  level: observation\n")
	replayMasked(t, candidate, storedOverride("trust.level", "autonomous"))
	merged := store.ConfigReadModel(candidate)
	merged["trust.level"] = map[string]any{"value": "autonomous", "source": "override"}
	markMaskedEntries(merged, candidate)
	entry := merged["trust.level"].(map[string]any)
	if entry["value"] != "observation" || entry["source"] != "override" ||
		entry["masked"] != true {
		t.Fatalf("trust.level entry = %v, want effective observation, override, masked",
			entry)
	}
	other := merged["collector.batch_size"].(map[string]any)
	if _, ok := other["masked"]; ok {
		t.Fatalf("an unmasked key carries the masked flag: %v", other)
	}
	if list := maskedResponse(config.DefaultConfig()); list == nil || len(list) != 0 {
		t.Fatalf("maskedResponse without masks = %#v, want an empty array", list)
	}
}
