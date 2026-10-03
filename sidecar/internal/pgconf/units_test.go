package pgconf

import "testing"

// Moved with parseMemory from internal/advisor (gucunits.go): a bare
// number is scaled by the GUC's base unit (G3-B01). With a 1-byte base
// the original expectations hold unchanged.
func TestParseValueMemoryByteBase(t *testing.T) {
	doc := GUCDoc{Unit: "bytes", BaseUnit: "B"}
	cases := map[string]float64{"256MB": 256 << 20, "1GB": 1 << 30, "512kB": 512 << 10,
		"1024": 1024}
	for in, want := range cases {
		got, err := ParseValue(in, doc)
		if err != nil || got != want {
			t.Errorf("ParseValue(%q) = %v,%v want %v", in, got, err, want)
		}
	}
}

// Time GUCs honor their base unit: checkpoint_timeout's bare number is
// seconds, autovacuum_vacuum_cost_delay's is milliseconds.
func TestParseValueTimeBaseUnit(t *testing.T) {
	seconds := GUCDoc{Unit: "ms", BaseUnit: "s"}
	millis := GUCDoc{Unit: "ms", BaseUnit: "ms"}
	cases := []struct {
		in   string
		doc  GUCDoc
		want float64
	}{
		{"900", seconds, 900_000},
		{"15min", seconds, 900_000},
		{"'5min'", seconds, 300_000},
		{"2", millis, 2},
		{"1s", millis, 1000},
	}
	for _, c := range cases {
		got, err := ParseValue(c.in, c.doc)
		if err != nil || got != c.want {
			t.Errorf("ParseValue(%q,%s) = %v,%v want %v", c.in, c.doc.BaseUnit, got, err, c.want)
		}
	}
	if _, err := ParseValue("5 minutes", seconds); err == nil {
		t.Error("unknown time unit accepted")
	}
}

// ToBaseUnits converts a requested value to pg_settings.setting's unit so
// a read-back compares like with like.
func TestToBaseUnits(t *testing.T) {
	cases := []struct {
		value, unit string
		want        float64
	}{
		{"64MB", "kB", 65536},
		{"'64MB'", "kB", 65536},
		{"65536", "kB", 65536},
		{"1MB", "8kB", 128},
		{"16384", "8kB", 16384},
		{"2GB", "MB", 2048},
		{"5min", "s", 300},
		{"900", "s", 900},
		{"2ms", "ms", 2},
		{"1s", "ms", 1000},
		{"0.9", "", 0.9},
		{"-1", "8kB", -1},
	}
	for _, c := range cases {
		got, err := ToBaseUnits(c.value, c.unit)
		if err != nil || got != c.want {
			t.Errorf("ToBaseUnits(%q,%q) = %v,%v want %v", c.value, c.unit, got, err, c.want)
		}
	}
	bad := []struct{ value, unit string }{
		{"64mb", "kB"},   // units are case-sensitive in PostgreSQL
		{"64MB", "s"},    // memory value for a time setting
		{"5min", "kB"},   // time value for a memory setting
		{"abc", ""},      // not a number
		{"", "kB"},       // empty
		{"64MB", "16MB"}, // unknown base unit
	}
	for _, c := range bad {
		if got, err := ToBaseUnits(c.value, c.unit); err == nil {
			t.Errorf("ToBaseUnits(%q,%q) = %v, want error", c.value, c.unit, got)
		}
	}
}
