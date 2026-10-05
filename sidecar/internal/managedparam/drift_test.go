package managedparam

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func setting(name, value, unit, source string, pending bool) collector.PGSetting {
	return collector.PGSetting{Name: name, Setting: value, Unit: unit, Source: source,
		PendingRestart: pending}
}

// Drift compares what the parameter group / flags configure with what
// PostgreSQL is running, so an operator sees changes waiting for a
// reboot, overrides, and console edits that never took effect.
func TestDetectDriftRDS(t *testing.T) {
	target := Target{Provider: "rds", ParameterGroup: "orders-pg16",
		ParameterGroupStatus: "in-sync", Known: true, Params: map[string]GroupParam{
			"shared_buffers":   {Value: "524288", Source: "user", ApplyType: "static"},
			"work_mem":         {Value: "65536", Source: "user", ApplyType: "dynamic"},
			"max_connections":  {Value: "500", Source: "user", ApplyType: "static"},
			"jit":              {Value: "0", Source: "user", ApplyType: "dynamic"},
			"random_page_cost": {Value: "1.1", Source: "user", ApplyType: "dynamic"},
			"effective_cache_size": {Value: "{DBInstanceClassMemory*3/32768}",
				Source: "user", ApplyType: "dynamic"},
			"maintenance_work_mem":    {Value: "1048576", Source: "engine-default"},
			"rds.logical_replication": {Value: "1", Source: "user", ApplyType: "static"},
		}}
	settings := []collector.PGSetting{
		setting("shared_buffers", "131072", "8kB", "configuration file", true),
		setting("work_mem", "4096", "kB", "database", false),
		setting("max_connections", "400", "", "configuration file", false),
		setting("jit", "off", "", "configuration file", false),
		setting("random_page_cost", "1.10", "", "configuration file", false),
		setting("effective_cache_size", "1", "8kB", "configuration file", false),
		setting("maintenance_work_mem", "65536", "kB", "default", false),
	}
	got := DetectDrift(target, settings)
	want := []Drift{
		{Parameter: "max_connections", Configured: "500", Running: "400", Kind: DriftMismatch},
		{Parameter: "shared_buffers", Configured: "524288", Running: "131072",
			Kind: DriftPendingReboot},
		{Parameter: "work_mem", Configured: "65536", Running: "4096", Kind: DriftOverridden},
	}
	if len(got) != len(want) {
		t.Fatalf("drift = %+v, want %+v", got, want)
	}
	for i := range want {
		g := got[i]
		if g.Parameter != want[i].Parameter || g.Configured != want[i].Configured ||
			g.Running != want[i].Running || g.Kind != want[i].Kind || g.Detail == "" {
			t.Fatalf("drift[%d] = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestDetectDriftGroupPendingRebootStatus(t *testing.T) {
	target := Target{Provider: "rds", ParameterGroupStatus: "pending-reboot", Known: true,
		Params: map[string]GroupParam{"max_connections": {Value: "500", Source: "user"}}}
	got := DetectDrift(target, []collector.PGSetting{
		setting("max_connections", "400", "", "configuration file", false)})
	if len(got) != 1 || got[0].Kind != DriftPendingReboot {
		t.Fatalf("drift = %+v", got)
	}
}

func TestDetectDriftCloudSQLFlags(t *testing.T) {
	target := Target{Provider: "cloud-sql", FlagsKnown: true, Known: true,
		Flags: map[string]string{"work_mem": "65536", "jit": "off",
			"cloudsql.iam_authentication": "on"}}
	got := DetectDrift(target, []collector.PGSetting{
		setting("work_mem", "65536", "kB", "configuration file", false),
		setting("jit", "on", "", "configuration file", false),
	})
	if len(got) != 1 || got[0].Parameter != "jit" || got[0].Kind != DriftMismatch {
		t.Fatalf("drift = %+v", got)
	}
}

func TestDetectDriftEmptyInputs(t *testing.T) {
	if got := DetectDrift(Target{}, nil); len(got) != 0 {
		t.Fatalf("empty = %+v", got)
	}
	unknown := Target{Provider: "rds", Params: map[string]GroupParam{
		"work_mem": {Value: "1", Source: "user"}}}
	if got := DetectDrift(unknown, []collector.PGSetting{
		setting("work_mem", "4096", "kB", "configuration file", false)}); len(got) != 0 {
		t.Fatalf("an unresolved target (Known=false) must not report drift: %+v", got)
	}
	flagsUnknown := Target{Provider: "cloud-sql", Known: true,
		Flags: map[string]string{"work_mem": "1"}}
	if got := DetectDrift(flagsUnknown, []collector.PGSetting{
		setting("work_mem", "4096", "kB", "configuration file", false)}); len(got) != 0 {
		t.Fatalf("unknown flags must not report drift: %+v", got)
	}
}
