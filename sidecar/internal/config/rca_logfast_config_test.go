package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// rca-06: rca.log_fast_path_interval_seconds paces the log fast path that
// opens critical log incidents between analyzer cycles.

// Default-value masking guard: with no config file the log fast path runs
// every 5 s, never 0 (disabled).
func TestRCALogFastPathDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.LogFastPathIntervalSeconds != 5 {
		t.Errorf("log_fast_path_interval_seconds = %d, want 5",
			cfg.RCA.LogFastPathIntervalSeconds)
	}
	if DefaultRCALogFastPathIntervalSeconds != 5 {
		t.Errorf("DefaultRCALogFastPathIntervalSeconds = %d, want 5",
			DefaultRCALogFastPathIntervalSeconds)
	}
	if got := cfg.RCA.LogFastPathInterval(); got != 5*time.Second {
		t.Errorf("LogFastPathInterval() = %s, want 5s", got)
	}
	if got := DefaultConfig().RCA.LogFastPathIntervalSeconds; got != 5 {
		t.Errorf("DefaultConfig interval = %d, want 5", got)
	}
}

// A partial rca section keeps the default; an explicit 0 disables.
func TestRCALogFastPathDefaults_PartialAndExplicitZero(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  enabled: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.LogFastPathIntervalSeconds != 5 {
		t.Fatalf("interval = %d, want default 5",
			cfg.RCA.LogFastPathIntervalSeconds)
	}
	off, err := loadRCAYAML(t, "rca:\n  log_fast_path_interval_seconds: 0\n")
	if err != nil {
		t.Fatalf("Load explicit 0: %v", err)
	}
	if off.RCA.LogFastPathIntervalSeconds != 0 {
		t.Fatalf("explicit 0 became %d", off.RCA.LogFastPathIntervalSeconds)
	}
	if got := off.RCA.LogFastPathInterval(); got != 0 {
		t.Fatalf("LogFastPathInterval() = %s, want 0 (disabled)", got)
	}
}

func TestRCALogFastPathInterval_Boundaries(t *testing.T) {
	cases := []struct {
		seconds int
		wantErr bool
	}{
		{-1, true},
		{0, false}, // 0 disables the log fast path
		{1, false},
		{5, false},
		{3600, false},
		{3601, true},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.seconds), func(t *testing.T) {
			cfg, err := loadRCAYAML(t, "rca:\n  log_fast_path_interval_seconds: "+
				strconv.Itoa(c.seconds)+"\n")
			if c.wantErr {
				if err == nil {
					t.Fatalf("interval %d accepted", c.seconds)
				}
				if !strings.Contains(err.Error(), "rca.log_fast_path_interval_seconds") {
					t.Fatalf("error does not name the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("interval %d rejected: %v", c.seconds, err)
			}
			want := time.Duration(c.seconds) * time.Second
			if got := cfg.RCA.LogFastPathInterval(); got != want {
				t.Fatalf("LogFastPathInterval() = %s, want %s", got, want)
			}
		})
	}
}

// A zero-value RCAConfig (built in code, not loaded) reports the fast
// path disabled rather than a negative or garbage duration.
func TestRCALogFastPathInterval_ZeroValueStruct(t *testing.T) {
	var r RCAConfig
	if got := r.LogFastPathInterval(); got != 0 {
		t.Fatalf("zero-value interval = %s, want 0", got)
	}
	r.LogFastPathIntervalSeconds = -3
	if got := r.LogFastPathInterval(); got != 0 {
		t.Fatalf("negative interval = %s, want 0", got)
	}
}

// The key is restart-bound like the lock-chain fast path: the ticker reads
// it once when the database's RCA wiring starts.
func TestRCALogFastPathLifecycle(t *testing.T) {
	got, ok := LookupFieldLifecycle("rca.log_fast_path_interval_seconds")
	if !ok {
		t.Fatal("rca.log_fast_path_interval_seconds missing from the lifecycle registry")
	}
	if got.Lifecycle != LifecycleRestart {
		t.Fatalf("lifecycle = %q, want %q", got.Lifecycle, LifecycleRestart)
	}
}
