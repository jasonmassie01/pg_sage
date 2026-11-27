package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/store"
)

// config-03 state transitions on real PostgreSQL: a stored UI override of
// trust.level=autonomous no longer silently beats an explicit YAML or env
// observation. Trust takes the most restrictive value, the masked key is
// logged once, and removing the file key brings the override back.

const maskingYAMLBase = "mode: standalone\npostgres:\n  host: db.example\n" +
	"  database: app\n  user: u\n"

// startMaskingSidecar stores the override, then runs the startup path on a
// YAML file and the control database.
func startMaskingSidecar(t *testing.T, yaml string) (*pgxpool.Pool, string) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	shared := preflightRuntimePool(t)
	previousPool, previousMeta, previousArgs := pool, globalMetaState, os.Args
	previousCtx := shutdownCtx
	t.Cleanup(func() {
		pool, globalMetaState, os.Args = previousPool, previousMeta, previousArgs
		shutdownCtx = previousCtx
	})
	reset := func() {
		_, _ = shared.Exec(context.Background(), `DELETE FROM sage.config
			WHERE database_id IS NULL AND key IN ('trust.level',
			'__pg_sage_config_generation')`)
	}
	reset()
	t.Cleanup(reset)
	writes := []store.ConfigOverrideWrite{{Key: "trust.level", Value: "autonomous"}}
	if _, err := store.NewConfigStore(shared).SetOverridesCAS(
		context.Background(), writes, 0, 0, 1,
	); err != nil {
		t.Fatalf("store the trust override: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeMaskingYAML(t, path, yaml)
	os.Args = []string{"pg_sage", "--config", path}
	loaded, err := loadFileConfigBase()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg, pool, globalMetaState, configController = loaded, shared, nil, nil
	shutdownCtx = context.Background()
	out := captureStderr(t, func() { err = initializeConfigController(shared) })
	if err != nil {
		t.Fatalf("initializeConfigController: %v", err)
	}
	return shared, out
}

func writeMaskingYAML(t *testing.T, path, yaml string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reloadMaskingYAML is the watcher: rewrite the file, load, apply.
func reloadMaskingYAML(t *testing.T, yaml string) string {
	t.Helper()
	writeMaskingYAML(t, os.Args[2], yaml)
	updated, err := loadConfigCandidate()
	if err != nil {
		t.Fatalf("load reload candidate: %v", err)
	}
	out := captureStderr(t, func() { err = applyWatchedConfig(updated) })
	if err != nil {
		t.Fatalf("apply reload: %v (log %q)", err, out)
	}
	return out
}

func maskedNoticeLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "trust.level") && strings.Contains(line, "masks") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestYAMLTrustReloadBeatsStoredAutonomousOverride(t *testing.T) {
	_, out := startMaskingSidecar(t, maskingYAMLBase)
	if got := configController.Desired().Config.Trust.Level; got != "autonomous" {
		t.Fatalf("startup trust.level = %q, want the override autonomous (YAML silent)", got)
	}
	if lines := maskedNoticeLines(out); len(lines) != 0 {
		t.Fatalf("startup logged a mask with no explicit file value: %q", lines)
	}

	out = reloadMaskingYAML(t, maskingYAMLBase+"trust:\n  level: observation\n")
	desired, active := configController.Desired(), configController.Active()
	if desired.Config.Trust.Level != "observation" || active.Config.Trust.Level != "observation" {
		t.Fatalf("after the YAML downgrade: desired %q active %q, want observation",
			desired.Config.Trust.Level, active.Config.Trust.Level)
	}
	if lines := maskedNoticeLines(out); len(lines) != 1 {
		t.Fatalf("masked notices = %q, want exactly one naming trust.level (log %q)",
			lines, out)
	}
	masked := configController.MaskedByOverride()
	if len(masked) != 1 || masked[0].Key != "trust.level" ||
		masked[0].OverrideValue != "autonomous" || masked[0].EffectiveValue != "observation" {
		t.Fatalf("MaskedByOverride = %+v, want trust.level held at observation", masked)
	}

	out = reloadMaskingYAML(t, maskingYAMLBase+"trust:\n  level: observation\n")
	if lines := maskedNoticeLines(out); len(lines) != 0 {
		t.Fatalf("the same masked pair was logged again: %q", lines)
	}

	reloadMaskingYAML(t, maskingYAMLBase)
	if got := configController.Desired().Config.Trust.Level; got != "autonomous" {
		t.Fatalf("after removing the YAML key trust.level = %q, want autonomous", got)
	}
	if masked := configController.MaskedByOverride(); len(masked) != 0 {
		t.Fatalf("MaskedByOverride = %+v after removing the YAML key", masked)
	}
}

func TestEnvTrustLevelBeatsStoredAutonomousOverrideAtStartup(t *testing.T) {
	t.Setenv("SAGE_TRUST_LEVEL", "observation")
	_, out := startMaskingSidecar(t, maskingYAMLBase)
	if got := cfg.Trust.Level; got != "observation" {
		t.Fatalf("startup cfg trust.level = %q, want SAGE_TRUST_LEVEL's observation", got)
	}
	if got := configController.Desired().Config.Trust.Level; got != "observation" {
		t.Fatalf("desired trust.level = %q, want observation", got)
	}
	if lines := maskedNoticeLines(out); len(lines) != 1 {
		t.Fatalf("startup masked notices = %q, want one (log %q)", lines, out)
	}
}
