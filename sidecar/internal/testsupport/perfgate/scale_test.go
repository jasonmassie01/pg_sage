package perfgate

import (
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestScaleFromEnvDefaultsToSmall(t *testing.T) {
	s, err := ScaleFromEnv(envOf(nil))
	if err != nil {
		t.Fatalf("default scale: %v", err)
	}
	if s != SmallScale() {
		t.Fatalf("default scale = %+v, want small %+v", s, SmallScale())
	}
	if s.Name != "small" || s.Tables() != 500 || s.HistoryRows != 20000 {
		t.Fatalf("small scale shape = %+v (tables %d)", s, s.Tables())
	}
}

func TestLargeScaleMatchesTheTargetCatalog(t *testing.T) {
	s, err := ScaleFromEnv(envOf(map[string]string{EnvScale: "large"}))
	if err != nil {
		t.Fatalf("large scale: %v", err)
	}
	if s.Schemas != 100 || s.CloneSchemas != 40 {
		t.Fatalf("schemas = %d (clones %d), want 100 (40)", s.Schemas, s.CloneSchemas)
	}
	if s.Tables() != 5000 || s.Indexes() != 15000 || s.Sequences() != 5000 {
		t.Fatalf("catalog = %d tables, %d indexes, %d sequences; want 5000/15000/5000",
			s.Tables(), s.Indexes(), s.Sequences())
	}
	if s.HistoryRows != 150000 {
		t.Fatalf("history rows = %d, want 150000", s.HistoryRows)
	}
}

func TestScaleFromEnvOverrides(t *testing.T) {
	s, err := ScaleFromEnv(envOf(map[string]string{
		EnvScale: "small", EnvTables: "1200", EnvHistoryRows: "5000",
	}))
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	// 1200 tables at 50 per schema is 24 schemas; clones keep their 40% share.
	if s.Schemas != 24 || s.Tables() != 1200 || s.CloneSchemas != 9 {
		t.Fatalf("override shape = %+v (tables %d)", s, s.Tables())
	}
	if s.HistoryRows != 5000 || s.Name != "small+override" {
		t.Fatalf("history/name = %d/%q", s.HistoryRows, s.Name)
	}
}

func TestScaleFromEnvRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown scale":     {EnvScale: "huge"},
		"non-numeric":       {EnvTables: "lots"},
		"zero tables":       {EnvTables: "0"},
		"negative history":  {EnvHistoryRows: "-1"},
		"below one schema":  {EnvTables: "10"},
		"history too small": {EnvHistoryRows: "99"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ScaleFromEnv(envOf(env))
			if err == nil {
				t.Fatalf("%v accepted", env)
			}
			if !strings.Contains(err.Error(), "PG_SAGE_PERF_") {
				t.Fatalf("error %q does not name the variable", err)
			}
		})
	}
}

func TestScaleValidate(t *testing.T) {
	bad := []Scale{
		{},
		{Schemas: 2, CloneSchemas: 3, TablesPerSchema: 1, IndexesPerTable: 1, HistoryRows: 100},
		{Schemas: 2, TablesPerSchema: 1, IndexesPerTable: 0, HistoryRows: 100},
		{Schemas: 2, TablesPerSchema: 1, IndexesPerTable: 1, HistoryRows: 0},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: %+v validated", i, s)
		}
	}
	if err := SmallScale().Validate(); err != nil {
		t.Fatalf("small scale invalid: %v", err)
	}
}

func TestTimingFromEnv(t *testing.T) {
	got, err := TimingFromEnv(envOf(nil))
	if err != nil {
		t.Fatalf("default timing: %v", err)
	}
	if got != DefaultTiming() || got.Interval.Seconds() != 15 ||
		got.Warmup.Seconds() != 45 || got.Window.Seconds() != 90 {
		t.Fatalf("default timing = %+v", got)
	}
	if c := got.Cycles(); c != 6 {
		t.Fatalf("cycles = %d, want 6", c)
	}
	got, err = TimingFromEnv(envOf(map[string]string{
		EnvInterval: "10s", EnvWarmup: "30s", EnvWindow: "2m",
	}))
	if err != nil || got.Cycles() != 12 {
		t.Fatalf("override timing = %+v (%v), cycles %d", got, err, got.Cycles())
	}
}

func TestTimingFromEnvRejectsInvalidValues(t *testing.T) {
	cases := []map[string]string{
		{EnvInterval: "soon"},
		{EnvInterval: "0s"},
		{EnvWindow: "5s"},                     // shorter than one interval
		{EnvWarmup: "-1s"},                    // negative
		{EnvInterval: "20s", EnvWarmup: "5s"}, // warmup shorter than one interval
	}
	for _, env := range cases {
		if _, err := TimingFromEnv(envOf(env)); err == nil ||
			!strings.Contains(err.Error(), "PG_SAGE_PERF_") {
			t.Errorf("%v: err = %v", env, err)
		}
	}
}

func TestHotTablesAreQualifiedAndBounded(t *testing.T) {
	got := SmallScale().HotTables()
	if len(got) != 10 || got[0] != "perf_app_000.t_0000" || got[9] != "perf_app_000.t_0009" {
		t.Fatalf("hot tables = %v", got)
	}
	small := Scale{Schemas: 1, TablesPerSchema: 3, IndexesPerTable: 1, HistoryRows: 100}
	if n := len(small.HotTables()); n != 3 {
		t.Fatalf("hot tables of a 3-table scale = %d", n)
	}
}
