package config_test

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-02b: parity for every registry key, generated from the tags.
// For each ranged KeySpec, YAML Load and the API accept Min and Max and
// reject the values just outside them, naming the key; for each enum key
// they accept every member and reject a non-member. The one declared
// asymmetry is zero:"default": a file may hold 0 (the default), and the
// API refuses to store it (removing the override restores the default).

// companions are the other settings a probe needs so that a cross-field
// rule does not decide the verdict instead of the range under test.
var companions = map[string]map[string]string{
	"verify.window_minutes":     {"verify.window_max_minutes": "1073741824"},
	"verify.window_max_minutes": {"verify.window_minutes": "1"},
	"rca.dedup_window_minutes":  {"rca.stale_after_hours": "8760"},
	// an enum member that needs a companion to load on its own
	"clone.provider=\"dle\"": {"clone.dle_endpoint": "https://dle.example"},
}

// apiRefused are enum members YAML accepts and the API deliberately does
// not: lakebase_branch needs the YAML-only clone.lakebase project binding,
// neon_branch the YAML-only clone.neon project and API key (lab-05b),
// aurora_clone the YAML-only clone.aurora region (lab-05c), rds_restore
// the YAML-only clone.rds region (lab-05d), cloudsql_clone the YAML-only
// clone.cloudsql project and instance (lab-05e), alloydb_restore the
// YAML-only clone.alloydb project, location and cluster (lab-05f),
// azure_restore the YAML-only clone.azure subscription, resource group and
// server (lab-05g).
var apiRefused = map[string]bool{"clone.provider=lakebase_branch": true,
	"clone.provider=neon_branch": true, "clone.provider=aurora_clone": true,
	"clone.provider=rds_restore": true, "clone.provider=cloudsql_clone": true,
	"clone.provider=alloydb_restore": true, "clone.provider=azure_restore": true}

func TestEveryRegistryRangeHasYAMLAndAPIParity(t *testing.T) {
	ranged := 0
	for _, s := range config.Registry() {
		if !s.HasRange {
			continue
		}
		ranged++
		for _, p := range rangeProbes(s) {
			checkParity(t, s, p.value, p.yamlOK, p.apiOK)
		}
	}
	if ranged < 80 {
		t.Fatalf("only %d ranged keys; the test lost its subject", ranged)
	}
}

func TestEveryRegistryEnumHasYAMLAndAPIParity(t *testing.T) {
	enums := 0
	for _, s := range config.Registry() {
		if len(s.Enum) == 0 {
			continue
		}
		enums++
		for _, v := range s.Enum {
			id := s.Key + "=" + v
			if apiRefused[id] {
				if err := store.ValidateConfigOverride(s.Key, v); err == nil {
					t.Errorf("API accepted %s", id)
				}
				continue
			}
			checkParity(t, s, yamlString(v), true, true)
		}
		checkParity(t, s, "not_a_member", false, false)
	}
	if enums != 5 {
		t.Fatalf("enum keys = %d, want 5", enums)
	}
}

type probe struct {
	value         string
	yamlOK, apiOK bool
}

// rangeProbes are Min, Max and the nearest values outside them; for a
// zero:"default" key, 0 loads from a file and the API refuses it. An open
// range (vacuum-11b) refuses Min and Max and accepts the values just inside.
func rangeProbes(s config.KeySpec) []probe {
	step := 1.0
	if !s.Integer {
		step = 0.001
	}
	format := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	if s.Open {
		return []probe{{format(s.Min + step), true, true}, {format(s.Max - step), true, true},
			{format(s.Min), false, false}, {format(s.Max), false, false},
			{format(s.Min - step), false, false}, {format(s.Max + step), false, false}}
	}
	probes := []probe{{format(s.Min), true, true}, {format(s.Max), true, true},
		{format(s.Max + step), false, false}}
	if below := s.Min - step; !s.ZeroDefault || below != 0 {
		probes = append(probes, probe{format(below), false, false})
	}
	if s.ZeroDefault {
		probes = append(probes, probe{"0", true, false}, probe{"-1", false, false})
	}
	return probes
}

func checkParity(t *testing.T, s config.KeySpec, value string, yamlOK, apiOK bool) {
	t.Helper()
	settings := map[string]string{s.Key: value}
	for _, id := range []string{s.Key, s.Key + "=" + value} {
		for k, v := range companions[id] {
			settings[k] = v
		}
	}
	_, yamlErr := config.Load([]string{"--config", writeRangeYAML(t, nestedYAML(settings))})
	verdicts := map[string]error{"YAML": yamlErr}
	if s.Overridable {
		verdicts["API"] = store.ValidateConfigOverride(s.Key, strings.Trim(value, `"`))
	}
	want := map[string]bool{"YAML": yamlOK, "API": apiOK}
	for path, err := range verdicts {
		if wantOK := want[path]; (err == nil) != wantOK {
			t.Errorf("%s %s=%s: err %v, want accepted=%v", path, s.Key, value, err, wantOK)
		} else if err != nil && !strings.Contains(err.Error(), s.Key) {
			t.Errorf("%s %s=%s: error %q does not name the key", path, s.Key, value, err)
		}
	}
}

// nestedYAML writes dot-notation settings as nested YAML sections.
func nestedYAML(settings map[string]string) string {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	var open []string
	for _, key := range keys {
		parts := strings.Split(key, ".")
		shared := 0
		for shared < len(open) && shared < len(parts)-1 && open[shared] == parts[shared] {
			shared++
		}
		for i := shared; i < len(parts)-1; i++ {
			b.WriteString(strings.Repeat("  ", i) + parts[i] + ":\n")
		}
		last := len(parts) - 1
		b.WriteString(strings.Repeat("  ", last) + parts[last] + ": " + settings[key] + "\n")
		open = parts[:last]
	}
	return b.String()
}

func yamlString(v string) string { return strconv.Quote(v) }

// Boundary (plan): int_min5 keys reject 4 and accept 5, and pct keys
// accept 0 and 100, on both paths.
func TestIntMin5AndPctBoundariesOnBothPaths(t *testing.T) {
	seen := map[string]int{}
	for _, s := range config.Registry() {
		switch s.Type {
		case "int_min5":
			checkParity(t, s, "4", false, false)
			checkParity(t, s, "5", true, true)
		case "pct":
			checkParity(t, s, "0", true, true)
			checkParity(t, s, "100", true, true)
		default:
			continue
		}
		seen[s.Type]++
	}
	if seen["int_min5"] < 4 || seen["pct"] < 6 {
		t.Fatalf("keys checked by type = %v, want at least 4 int_min5 and 6 pct", seen)
	}
}

// Nil/empty: an empty YAML file loads with the defaults.
func TestLoadAcceptsAnEmptyFile(t *testing.T) {
	path := writeRangeYAML(t, "")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load(empty file) = %v", err)
	}
	if cfg.Collector.IntervalSeconds != 60 || cfg.Trust.Level != "observation" {
		t.Fatalf("empty file lost defaults: interval=%d trust=%q",
			cfg.Collector.IntervalSeconds, cfg.Trust.Level)
	}
}

// The ranges that moved in config-02b, pinned so a later tag edit is
// visible (the full list is in the release note).
func TestRangesThatMovedInConfig02b(t *testing.T) {
	for _, tc := range []struct {
		key, value    string
		yamlOK, apiOK bool
	}{
		{"llm.timeout_seconds", "0", false, false},                 // YAML moved
		{"retention.snapshots_days", "0", false, false},            // YAML moved
		{"safety.lock_timeout_ms", "0", false, false},              // YAML moved
		{"analyzer.cache_hit_ratio_warning", "1.01", false, false}, // YAML moved
		{"llm.token_budget_daily", "0", true, true},                // API moved: 0 = no cap
		{"alerting.cooldown_minutes", "0", true, true},             // API moved: floors only
		{"analyzer.self_cost_budget_ms", "3600001", false, false},  // API moved
		{"rca.dedup_window_minutes", "525601", false, false},       // API moved
		{"safety.host_cpu_ceiling_pct", "0", true, false},          // unchanged: file default
		{"trust.cascade_cooldown_cycles", "0", true, false},        // unchanged: file default
		// The default threshold is 1 GiB, the generic int ceiling: a busy
		// primary must be able to raise it (review of config-02b).
		{"analyzer.slow_slot_retained_bytes", "10737418240", true, true},
		{"analyzer.slow_slot_retained_bytes", "1099511627777", false, false},
	} {
		s, ok := config.LookupKey(tc.key)
		if !ok {
			t.Fatalf("%s is not in the registry", tc.key)
		}
		checkParity(t, s, tc.value, tc.yamlOK, tc.apiOK)
	}
}
