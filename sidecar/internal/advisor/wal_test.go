package advisor

import (
	"context"
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// walFactsSnapshot has every setting shape the WAL prompt renders: a
// memory setting in 8kB pages with a pending restart, a time, a ratio,
// an enum, and a setting the WAL advisor does not read.
func walFactsSnapshot() *collector.Snapshot {
	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "max_wal_size", Setting: "1024", Unit: "MB"},
				{Name: "min_wal_size", Setting: "80", Unit: "MB"},
				{Name: "checkpoint_timeout", Setting: "300", Unit: "s"},
				{Name: "checkpoint_completion_target", Setting: "0.9"},
				{Name: "wal_buffers", Setting: "512", Unit: "8kB", PendingRestart: true},
				{Name: "wal_level", Setting: "logical"},
				{Name: "work_mem", Setting: "4096", Unit: "kB"},
			},
			WALPosition: "0/3000148",
		},
		System: collector.SystemStats{TotalCheckpoints: 400},
		Tables: []collector.TableStats{{RelName: "cache", Relpersistence: "u"},
			{RelName: "orders", Relpersistence: "p"}},
	}
	return snap
}

func walPrompt(t *testing.T, env promptEnv) capturedPrompt {
	t.Helper()
	mgr, capture := capturingManager(t, "[]")
	ctx := context.WithValue(context.Background(), promptEnvKey{}, env)
	snap := walFactsSnapshot()
	if _, err := analyzeWAL(ctx, mgr, snap, config.DefaultConfig(), noopLog); err != nil {
		t.Fatalf("analyzeWAL: %v", err)
	}
	return onlyPrompt(t, capture())
}

// wal_buffers is 512 pages of 8kB: the prompt must say 4MB, never "5128kB".
func TestWALPrompt_ComputedFacts(t *testing.T) {
	want := map[string]string{
		"max_wal_size":                 "1024MB",
		"min_wal_size":                 "80MB",
		"checkpoint_timeout":           "300s",
		"checkpoint_completion_target": "0.9",
		"wal_buffers":                  "4MB (pending_restart=true)",
		"wal_level":                    "logical",
		// advisors-05: the 60 s "delta since last" is gone; without the
		// window's history every window fact is stated unknown.
		"Total checkpoints since the statistics epoch": "400",
		"Advisor window":            "24h0m0s, unknown (no snapshot history is available)",
		"Checkpoints in the window": "unknown",
		"WAL generated":             "unknown",
		"Lowest max_wal_size":       "unknown (do not recommend a max_wal_size value)",
		"Highest max_wal_size":      "unknown",
		"WAL position":              "0/3000148",
		"Note": "1 unlogged table(s) detected. Unlogged tables do NOT generate WAL, " +
			"so low WAL activity may be expected if writes target them.",
	}
	got := promptFacts(walPrompt(t, promptEnv{platform: "rds", pgMajor: 17}).user)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WAL facts:\n got %v\nwant %v", got, want)
	}
}

func TestWALPrompt_PlatformAndDocRangeFacts(t *testing.T) {
	ranges := map[string]string{
		"max_wal_size": "512MB..64GB", "min_wal_size": "32MB..32GB",
		"checkpoint_timeout":           "60000ms..3600000ms",
		"checkpoint_completion_target": "0.5..0.95", "wal_buffers": "1MB..1GB",
	}
	units := map[string]string{"max_wal_size": "MB", "min_wal_size": "MB",
		"checkpoint_timeout": "s", "wal_buffers": "8kB"}
	cases := []struct {
		env            promptEnv
		platform, kind string
		notes          map[string]string
	}{
		{promptEnv{platform: "cloud-sql", pgMajor: 17}, "cloud-sql", "managed service",
			map[string]string{"max_wal_size": "17"}},
		{promptEnv{platform: "self-managed", pgMajor: 16}, "self-managed",
			"ALTER SYSTEM is available; restart-only settings need an " +
				"operator-scheduled restart", map[string]string{}},
		{promptEnv{platform: "unknown"}, "unknown",
			"the provider probe has not identified the host", map[string]string{}},
	}
	for _, c := range cases {
		f := readSystemFacts(walPrompt(t, c.env).system)
		if f.platform != c.platform || f.kind != c.kind {
			t.Errorf("%+v: platform %q (%q), want %q (%q)", c.env, f.platform, f.kind,
				c.platform, c.kind)
		}
		if !reflect.DeepEqual(f.ranges, ranges) || !reflect.DeepEqual(f.units, units) {
			t.Errorf("%+v: ranges %v units %v", c.env, f.ranges, f.units)
		}
		if !reflect.DeepEqual(f.notes, c.notes) {
			t.Errorf("%+v: version notes %v, want %v", c.env, f.notes, c.notes)
		}
	}
}

func TestWALSettingText(t *testing.T) {
	cases := []struct {
		s    collector.PGSetting
		want string
	}{
		{collector.PGSetting{Setting: "512", Unit: "8kB"}, "4MB"},
		{collector.PGSetting{Setting: "16384", Unit: "8kB"}, "128MB"},
		{collector.PGSetting{Setting: "3", Unit: "8kB"}, "24kB"},
		{collector.PGSetting{Setting: "-1", Unit: "8kB"}, "-1"},
		{collector.PGSetting{Setting: "1024", Unit: "MB"}, "1024MB"},
		{collector.PGSetting{Setting: "300", Unit: "s"}, "300s"},
		{collector.PGSetting{Setting: "on"}, "on"},
		{collector.PGSetting{Setting: "abc", Unit: "8kB"}, "abc8kB"},
		{collector.PGSetting{}, ""},
	}
	for _, c := range cases {
		if got := settingText(c.s); got != c.want {
			t.Errorf("settingText(%q, %q) = %q, want %q", c.s.Setting, c.s.Unit, got, c.want)
		}
	}
}

func TestCountUnloggedTables(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "orders", Relpersistence: "p"},
			{RelName: "cache", Relpersistence: "u"},
			{RelName: "sessions", Relpersistence: "u"},
			{RelName: "temp_data", Relpersistence: "t"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 2 {
		t.Errorf("countUnloggedTables = %d, want 2", got)
	}
}

func TestCountUnloggedTables_None(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "orders", Relpersistence: "p"},
			{RelName: "items", Relpersistence: "p"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 0 {
		t.Errorf("countUnloggedTables = %d, want 0", got)
	}
}

func TestCountUnloggedTables_EmptySnapshot(t *testing.T) {
	snap := &collector.Snapshot{}
	got := countUnloggedTables(snap)
	if got != 0 {
		t.Errorf("countUnloggedTables = %d, want 0", got)
	}
}
