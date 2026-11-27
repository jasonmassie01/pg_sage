package config_test

import (
	"path/filepath"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-02 review: the shipped example and test-harness configs must keep
// loading under the shared ranges. The browser compose fixture and the
// full-surface harness set unused_index_window_days: 0 before config-02 and
// would have stopped the sidecar at startup.
func TestShippedFixtureConfigsLoadUnderTheSharedRanges(t *testing.T) {
	t.Setenv("SAGE_DATABASE_URL", "")
	t.Setenv("SAGE_MODE", "")
	t.Setenv("SAGE_CONFIG_PATH", "")
	root := filepath.Join("..", "..", "..")
	for _, rel := range []string{
		"config.example.yaml",
		"sidecar/config.example.yaml",
		"test-fixtures/config.test.yaml",
		"test-fixtures/full_surface/verify_config.yaml",
		"test-fixtures/full_surface/verify_config_approval.yaml",
		"test-fixtures/full_surface/verify_config_walkthrough.yaml",
		"sidecar/tests/demo/config_demo.yaml",
		"sidecar/tests/integration/config_test.yaml",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		cfg, err := config.Load([]string{"--config", path})
		if err != nil {
			t.Errorf("%s does not load: %v", rel, err)
			continue
		}
		if cfg.Analyzer.UnusedIndexWindowDays < 1 || cfg.Trust.RollbackCooldownDays < 1 {
			t.Errorf("%s loaded unused_index_window_days=%d rollback_cooldown_days=%d, "+
				"want both at least 1", rel, cfg.Analyzer.UnusedIndexWindowDays,
				cfg.Trust.RollbackCooldownDays)
		}
	}
}
