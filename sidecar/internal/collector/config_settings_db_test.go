package collector

import (
	"context"
	"strings"
	"testing"
)

// market-15: the config snapshot collects PostgreSQL 18's I/O settings.
// Availability differs by version, and a name the server lacks returns no
// row rather than an error.

// No concurrent access tests: one read-only catalog query per call.

func TestConfigSettingsQueryListsTheIOSettings(t *testing.T) {
	for _, name := range []string{"io_method", "io_workers", "io_combine_limit",
		"maintenance_io_concurrency", "effective_io_concurrency"} {
		if !strings.Contains(configSettingsSQL, "'"+name+"'") {
			t.Errorf("configSettingsSQL does not collect %s", name)
		}
	}
}

func TestReadConfigSettings_IOSettingsByVersion(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	major := serverVersion(t, pool) / 10000
	var cs ConfigSnapshot
	if err := readConfigSettings(ctx, pool, &cs); err != nil {
		t.Fatalf("readConfigSettings on PG%d: %v", major, err)
	}
	got := map[string]PGSetting{}
	for _, s := range cs.PGSettings {
		got[s.Name] = s
	}
	want := map[string]bool{
		"maintenance_io_concurrency": major >= 13,
		"effective_io_concurrency":   true,
		"io_combine_limit":           major >= 17,
		"io_method":                  major >= 18,
		"io_workers":                 major >= 18,
	}
	for name, present := range want {
		s, ok := got[name]
		if ok != present {
			t.Errorf("PG%d: %s collected = %v, want %v", major, name, ok, present)
			continue
		}
		if ok && (s.Setting == "" || s.Source == "" || s.Context == "") {
			t.Errorf("PG%d: %s row = %+v, want setting, source and context", major, name, s)
		}
	}
	if s, ok := got["io_combine_limit"]; ok && s.Unit != "8kB" {
		t.Errorf("io_combine_limit unit = %q, want 8kB", s.Unit)
	}
	if s, ok := got["io_method"]; ok && s.Context != "postmaster" {
		t.Errorf("io_method context = %q, want postmaster", s.Context)
	}
	if _, ok := got["work_mem"]; !ok {
		t.Error("work_mem is missing: the settings read lost its other rows")
	}
}
