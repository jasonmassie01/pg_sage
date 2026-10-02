package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Sage SRE M0 config keys: rca.lock_chain_interval_seconds (fast path)
// and rca.narration_enabled (LLM narration kill switch).

func loadRCAYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "mode: standalone\npostgres:\n  host: db.example\n" + body
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load([]string{"--config", path})
}

// Default-value masking guard: with no config file at all the fast path
// runs every 60 s and narration stays off.
func TestRCASREDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.LockChainIntervalSeconds != 60 {
		t.Errorf("lock_chain_interval_seconds = %d, want 60",
			cfg.RCA.LockChainIntervalSeconds)
	}
	if DefaultRCALockChainIntervalSeconds != 60 {
		t.Errorf("DefaultRCALockChainIntervalSeconds = %d, want 60",
			DefaultRCALockChainIntervalSeconds)
	}
	if !cfg.RCA.NarrationEnabled {
		t.Error("narration_enabled must default to true")
	}
	if got := cfg.RCA.LockChainInterval(); got.Seconds() != 60 {
		t.Errorf("LockChainInterval() = %s, want 60s", got)
	}
}

// A partial rca section keeps the default interval.
func TestRCASREDefaults_PartialSectionKeepsInterval(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  enabled: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.LockChainIntervalSeconds != 60 {
		t.Fatalf("interval = %d, want default 60",
			cfg.RCA.LockChainIntervalSeconds)
	}
}

func TestRCALockChainInterval_Boundaries(t *testing.T) {
	cases := []struct {
		seconds int
		wantErr bool
	}{
		{-1, true},
		{0, false}, // 0 disables the fast path
		{1, true},
		{9, true},
		{10, false},
		{59, false},
		{60, false},
		{3600, false},
		{3601, true},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.seconds), func(t *testing.T) {
			cfg, err := loadRCAYAML(t, "rca:\n  lock_chain_interval_seconds: "+
				strconv.Itoa(c.seconds)+"\n")
			if c.wantErr {
				if err == nil {
					t.Fatalf("interval %d accepted, want error", c.seconds)
				}
				if !strings.Contains(err.Error(),
					"rca.lock_chain_interval_seconds") {
					t.Fatalf("error must name the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("interval %d rejected: %v", c.seconds, err)
			}
			if cfg.RCA.LockChainIntervalSeconds != c.seconds {
				t.Fatalf("interval = %d, want %d",
					cfg.RCA.LockChainIntervalSeconds, c.seconds)
			}
		})
	}
}

func TestRCALockChainInterval_ZeroMeansDisabled(t *testing.T) {
	r := RCAConfig{LockChainIntervalSeconds: 0}
	if r.LockChainInterval() != 0 {
		t.Fatalf("LockChainInterval() = %s, want 0 (disabled)",
			r.LockChainInterval())
	}
	r.LockChainIntervalSeconds = 59
	if r.LockChainInterval().Seconds() != 59 {
		t.Fatalf("LockChainInterval() = %s, want 59s", r.LockChainInterval())
	}
}

func TestRCANarrationEnabled_ParsesFromYAML(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  narration_enabled: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RCA.NarrationEnabled {
		t.Fatal("narration_enabled: true was not loaded")
	}
}

func TestRCASREKeys_HaveDocTags(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{
		"rca.lock_chain_interval_seconds", "rca.narration_enabled",
	} {
		if strings.TrimSpace(docs[key]) == "" {
			t.Errorf("%s has no doc tag", key)
		}
	}
}
