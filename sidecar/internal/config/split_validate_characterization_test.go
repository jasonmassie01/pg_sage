package config

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// config-30 characterization of validate(): one bad value per section
// pins today's exact error string, and pairs of bad values pin which
// error wins (the validation order decides what an operator sees first).

type splitCase struct {
	name   string
	mutate func(*Config)
	want   string
}

const portError = "postgres.port must be from 1 to 65535, got %d (standalone " +
	"mode connects with the postgres settings; --meta-db selects a meta-db fleet)"

func splitValidateCases() []splitCase {
	return []splitCase{
		{"postgres port", func(c *Config) { c.Postgres.Port = 0 },
			"postgres.port must be from 1 to 65535, got 0 (standalone mode connects " +
				"with the postgres settings; --meta-db selects a meta-db fleet)"},
		{"collector interval", func(c *Config) { c.Collector.IntervalSeconds = 0 },
			"collector.interval_seconds must be positive"},
		{"collector batch", func(c *Config) { c.Collector.BatchSize = 0 },
			"collector.batch_size must be positive"},
		{"collector max queries", func(c *Config) { c.Collector.MaxQueries = -1 },
			"collector.max_queries must be positive"},
		{"analyzer interval", func(c *Config) { c.Analyzer.IntervalSeconds = 0 },
			"analyzer.interval_seconds must be positive"},
		{"analyzer slow threshold", func(c *Config) { c.Analyzer.SlowQueryThresholdMs = -1 },
			"analyzer.slow_query_threshold_ms must be non-negative"},
		{"analyzer slow min calls", func(c *Config) { c.Analyzer.SlowQueryMinCalls = -2 },
			"analyzer.slow_query_min_calls must be non-negative, got -2"},
		{"trust level", func(c *Config) { c.Trust.Level = "bogus" },
			`trust.level must be observation, advisory, or autonomous (got "bogus")`},
		{"safety cpu zero", func(c *Config) { c.Safety.CPUCeilingPct = 0 },
			"safety.cpu_ceiling_pct must be 1-100"},
		{"safety cpu over", func(c *Config) { c.Safety.CPUCeilingPct = 101 },
			"safety.cpu_ceiling_pct must be 1-100"},
		{"safety query timeout", func(c *Config) { c.Safety.QueryTimeoutMs = 0 },
			"safety.query_timeout_ms must be positive"},
		{"fleet without databases", func(c *Config) { c.Mode = "fleet" },
			"fleet mode requires at least one database"},
		{"fleet database without host", func(c *Config) {
			c.Mode = "fleet"
			c.Databases = []DatabaseConfig{{Name: "app"}}
		}, `databases[0] "app": host must not be empty`},
	}
}

// Two bad values at once: the earlier check in today's order wins.
func splitOrderCases() []splitCase {
	return []splitCase{
		{"port before collector", func(c *Config) {
			c.Postgres.Port = 70000
			c.Collector.IntervalSeconds = 0
		}, "postgres.port must be from 1 to 65535, got 70000 (standalone mode " +
			"connects with the postgres settings; --meta-db selects a meta-db fleet)"},
		{"collector interval before batch", func(c *Config) {
			c.Collector.IntervalSeconds = 0
			c.Collector.BatchSize = 0
		}, "collector.interval_seconds must be positive"},
		{"collector before analyzer", func(c *Config) {
			c.Collector.MaxQueries = 0
			c.Analyzer.IntervalSeconds = 0
		}, "collector.max_queries must be positive"},
		{"analyzer before trust", func(c *Config) {
			c.Analyzer.SlowQueryThresholdMs = -1
			c.Trust.Level = "bogus"
		}, "analyzer.slow_query_threshold_ms must be non-negative"},
		{"trust before safety", func(c *Config) {
			c.Trust.Level = "bogus"
			c.Safety.CPUCeilingPct = 0
		}, `trust.level must be observation, advisory, or autonomous (got "bogus")`},
		{"safety cpu before query timeout", func(c *Config) {
			c.Safety.CPUCeilingPct = 0
			c.Safety.QueryTimeoutMs = 0
		}, "safety.cpu_ceiling_pct must be 1-100"},
		{"safety before fleet", func(c *Config) {
			c.Safety.QueryTimeoutMs = 0
			c.Mode = "fleet"
		}, "safety.query_timeout_ms must be positive"},
	}
}

func runSplitCases(t *testing.T, cases []splitCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(cfg)
			err := cfg.validate()
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSplit_DefaultConfigValidates(t *testing.T) {
	if err := DefaultConfig().validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestSplit_ValidateOneBadValuePerSection(t *testing.T) {
	runSplitCases(t, splitValidateCases())
}

func TestSplit_ValidateFirstFailureWins(t *testing.T) {
	runSplitCases(t, splitOrderCases())
}

func TestSplit_ValidatePostgresPortBoundaries(t *testing.T) {
	for _, tc := range []struct {
		port int
		ok   bool
	}{{0, false}, {1, true}, {65535, true}, {65536, false}} {
		cfg := DefaultConfig()
		cfg.Postgres.Port = tc.port
		err := cfg.validatePostgresPort()
		if tc.ok != (err == nil) {
			t.Fatalf("port %d: err = %v, want ok=%v", tc.port, err, tc.ok)
		}
		if err != nil && err.Error() != fmt.Sprintf(portError, tc.port) {
			t.Fatalf("port %d: err = %q", tc.port, err)
		}
	}
	// Fleet and meta-db modes take their connections from elsewhere.
	cfg := DefaultConfig()
	cfg.Postgres.Port = 0
	cfg.MetaDB = "postgres://meta"
	if err := cfg.validatePostgresPort(); err != nil {
		t.Fatalf("meta-db with port 0: %v", err)
	}
	cfg.MetaDB, cfg.Mode = "", "fleet"
	if err := cfg.validatePostgresPort(); err != nil {
		t.Fatalf("fleet with port 0: %v", err)
	}
}

// The collector interval has two checks: the inline "must be positive"
// (0) runs before the value registry's 5-second floor (1 to 4).
func TestSplit_CollectorIntervalBoundary(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    string
	}{
		{0, "collector.interval_seconds must be positive"},
		{1, "collector.interval_seconds must be 5-1073741824, got 1"},
		{4, "collector.interval_seconds must be 5-1073741824, got 4"},
		{5, ""},
	} {
		cfg := DefaultConfig()
		cfg.Collector.IntervalSeconds = tc.seconds
		got := ""
		if err := cfg.validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Fatalf("interval %d: validate() = %q, want %q", tc.seconds, got, tc.want)
		}
	}
	cfg := DefaultConfig()
	cfg.Collector.IntervalSeconds = 5
	if got := cfg.Collector.Interval(); got != 5*time.Second {
		t.Fatalf("Interval() = %v, want 5s", got)
	}
}

func TestSplit_ValidateModeErrors(t *testing.T) {
	for _, tc := range []struct{ mode, meta, want string }{
		{"standalone", "", ""},
		{"fleet", "", ""},
		{"meta", "postgres://meta", ""},
		{"meta", "", `mode "meta" requires --meta-db (meta_db)`},
		{"standalone", "postgres://meta", "mode standalone cannot be combined with " +
			"meta_db; drop mode (meta is inferred) or set mode: meta"},
		{"extension", "", `mode "extension" was removed with the C extension: use ` +
			"standalone for one database, fleet for several, or --meta-db for a " +
			"meta-db fleet"},
		{"", "", `invalid mode "": must be 'standalone', 'fleet', or 'meta'`},
	} {
		err := validateMode(&Config{Mode: tc.mode, MetaDB: tc.meta})
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Fatalf("validateMode(%q, %q) = %q, want %q", tc.mode, tc.meta, got, tc.want)
		}
	}
}

// Concurrency: Load takes no hot-reload lock of its own, so it completes
// while a writer holds the lock and while readers hold it concurrently.
func TestSplit_LoadTakesNoHotReloadLock(t *testing.T) {
	isolateSplitEnv(t)
	LockForHotReload()
	done := make(chan error, 1)
	go func() {
		_, err := Load([]string{})
		done <- err
	}()
	select {
	case err := <-done:
		UnlockForHotReload()
		if err != nil {
			t.Fatalf("Load under a held write lock: %v", err)
		}
	case <-time.After(30 * time.Second):
		UnlockForHotReload()
		t.Fatal("Load blocked while the hot-reload write lock was held")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			RLockForHotReload()
			time.Sleep(5 * time.Millisecond)
			RUnlockForHotReload()
		}()
		go func() {
			defer wg.Done()
			cfg, err := Load([]string{})
			if err == nil && cfg.Collector.IntervalSeconds != 60 {
				err = errors.New("concurrent Load lost the collector default")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Load: %v", err)
		}
	}
}
