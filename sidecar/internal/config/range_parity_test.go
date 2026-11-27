package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-02: the YAML range checks mirror the API's types for these keys.
// The store registry is the reference: YAML must reject exactly the values
// a PUT /api/v1/config/global rejects.

type rangedKey struct {
	key string
	set func(*config.Config, int)
	get func(*config.Config) int
}

var phase1RangedKeys = []rangedKey{
	{"analyzer.unused_index_window_days",
		func(c *config.Config, v int) { c.Analyzer.UnusedIndexWindowDays = v },
		func(c *config.Config) int { return c.Analyzer.UnusedIndexWindowDays }},
	{"analyzer.seq_scan_min_rows",
		func(c *config.Config, v int) { c.Analyzer.SeqScanMinRows = v },
		func(c *config.Config) int { return c.Analyzer.SeqScanMinRows }},
	{"safety.ddl_timeout_seconds",
		func(c *config.Config, v int) { c.Safety.DDLTimeoutSeconds = v },
		func(c *config.Config) int { return c.Safety.DDLTimeoutSeconds }},
	{"trust.rollback_threshold_pct",
		func(c *config.Config, v int) { c.Trust.RollbackThresholdPct = v },
		func(c *config.Config) int { return c.Trust.RollbackThresholdPct }},
	{"trust.rollback_window_minutes",
		func(c *config.Config, v int) { c.Trust.RollbackWindowMinutes = v },
		func(c *config.Config) int { return c.Trust.RollbackWindowMinutes }},
	{"trust.rollback_cooldown_days",
		func(c *config.Config, v int) { c.Trust.RollbackCooldownDays = v },
		func(c *config.Config) int { return c.Trust.RollbackCooldownDays }},
	{"advisor.interval_seconds",
		func(c *config.Config, v int) { c.Advisor.IntervalSeconds = v },
		func(c *config.Config) int { return c.Advisor.IntervalSeconds }},
	{"alerting.check_interval_seconds",
		func(c *config.Config, v int) { c.Alerting.CheckIntervalSeconds = v },
		func(c *config.Config) int { return c.Alerting.CheckIntervalSeconds }},
	{"schema_lint.scan_interval_minutes",
		func(c *config.Config, v int) { c.SchemaLint.ScanIntervalMinutes = v },
		func(c *config.Config) int { return c.SchemaLint.ScanIntervalMinutes }},
	{"migration.poll_interval_seconds",
		func(c *config.Config, v int) { c.Migration.PollIntervalSeconds = v },
		func(c *config.Config) int { return c.Migration.PollIntervalSeconds }},
}

// parityValues spans every boundary the API types use: int_pos (1),
// int_min5 (5), pct (0-100) and the shared 1<<30 ceiling.
var parityValues = []int{-5, -1, 0, 1, 4, 5, 100, 101, 1 << 30, 1<<30 + 1}

func TestYAMLRangesMatchAPIRanges(t *testing.T) {
	for _, rk := range phase1RangedKeys {
		for _, v := range parityValues {
			apiErr := store.ValidateConfigOverride(rk.key, strconv.Itoa(v))
			cfg := config.DefaultConfig()
			rk.set(cfg, v)
			yamlErr := cfg.Validate()
			if (apiErr == nil) != (yamlErr == nil) {
				t.Errorf("%s=%d: API err %v, YAML err %v; want the same verdict",
					rk.key, v, apiErr, yamlErr)
				continue
			}
			if yamlErr != nil && !strings.Contains(yamlErr.Error(), rk.key) {
				t.Errorf("%s=%d: YAML error %q does not name the key",
					rk.key, v, yamlErr)
			}
		}
	}
}

// The plan's literal cases: 0 and -5 fail on both paths for the int_pos
// keys, and the API's own minimum is accepted.
func TestYAMLRejectsZeroAndNegativeForPositiveKeys(t *testing.T) {
	for _, rk := range phase1RangedKeys {
		if rk.key == "trust.rollback_threshold_pct" {
			continue // a percentage: 0 is a valid threshold on both paths
		}
		for _, v := range []int{0, -5} {
			cfg := config.DefaultConfig()
			rk.set(cfg, v)
			if err := cfg.Validate(); err == nil {
				t.Errorf("%s=%d validated, want it rejected", rk.key, v)
			}
		}
	}
}

// Default masking: with no YAML at all, every new range is satisfied and
// every default is a value the API itself would accept on save.
func TestDefaultConfigPassesEveryNewRange(t *testing.T) {
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig() fails validation: %v", err)
	}
	for _, rk := range phase1RangedKeys {
		value := strconv.Itoa(rk.get(cfg))
		if err := store.ValidateConfigOverride(rk.key, value); err != nil {
			t.Errorf("default %s=%s is outside the API range: %v", rk.key, value, err)
		}
	}
}

func TestLoadRejectsZeroUnusedIndexWindowNamingTheKey(t *testing.T) {
	path := writeRangeYAML(t, "analyzer:\n  unused_index_window_days: 0\n")
	_, err := config.Load([]string{"--config", path})
	if err == nil {
		t.Fatal("Load accepted analyzer.unused_index_window_days: 0")
	}
	if !strings.Contains(err.Error(), "analyzer.unused_index_window_days") {
		t.Fatalf("Load error %q does not name analyzer.unused_index_window_days", err)
	}
}

func TestLoadRejectsZeroDDLTimeoutNamingTheKey(t *testing.T) {
	path := writeRangeYAML(t, "safety:\n  ddl_timeout_seconds: 0\n")
	_, err := config.Load([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "safety.ddl_timeout_seconds") {
		t.Fatalf("Load error = %v, want it to name safety.ddl_timeout_seconds", err)
	}
}

func TestLoadAcceptsTheMinimumUnusedIndexWindow(t *testing.T) {
	path := writeRangeYAML(t, "analyzer:\n  unused_index_window_days: 1\n")
	cfg, err := config.Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load rejected unused_index_window_days: 1: %v", err)
	}
	if cfg.Analyzer.UnusedIndexWindowDays != 1 {
		t.Fatalf("unused_index_window_days = %d, want 1",
			cfg.Analyzer.UnusedIndexWindowDays)
	}
}

// writeRangeYAML writes a standalone config with the given extra sections
// into a fresh directory and isolates Load from the developer's shell.
func writeRangeYAML(t *testing.T, extra string) string {
	t.Helper()
	t.Setenv("SAGE_DATABASE_URL", "")
	t.Setenv("SAGE_MODE", "")
	t.Setenv("SAGE_CONFIG_PATH", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "mode: standalone\npostgres:\n  host: db.example\n  port: 5432\n" +
		"  user: sage\n  database: app\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
