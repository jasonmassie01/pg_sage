package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// OperatorSetPaths: which canonical keys the operator wrote in the YAML
// file for one database. An operator-set value always wins over a derived
// one, so a key must count as set exactly when the operator wrote it.

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func TestOperatorSetPathsNestedLeaves(t *testing.T) {
	raw := []byte(`
collector:
  interval_seconds: 120
safety:
  query_timeout_ms: 900
sre:
  runways:
    sequence_interval_seconds: 1200
  detectors: {temp_file_mb: 2048}
alerting:
  routes:
    - channel: ops
api:
  trusted_proxies: [10.0.0.1]
`)
	got, err := OperatorSetPaths(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alerting.routes", "api.trusted_proxies", "collector.interval_seconds",
		"safety.query_timeout_ms", "sre.detectors.temp_file_mb",
		"sre.runways.sequence_interval_seconds"}
	if !reflect.DeepEqual(sortedKeys(got), want) {
		t.Fatalf("paths %v, want %v", sortedKeys(got), want)
	}
	if got["collector"] || got["sre.runways"] {
		t.Fatal("a section mapping counted as a set key")
	}
}

func TestOperatorSetPathsEmptyAndNullSections(t *testing.T) {
	for name, raw := range map[string]string{
		"empty document":  "",
		"comment only":    "# nothing\n",
		"empty mapping":   "collector: {}\n",
		"null section":    "collector:\n",
		"explicit null":   "collector: null\n",
		"blank sre block": "sre:\n  runways: {}\n",
	} {
		got, err := OperatorSetPaths([]byte(raw), "")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(sortedKeys(got)) != 0 {
			t.Errorf("%s: paths %v, want none", name, sortedKeys(got))
		}
	}
}

// A key explicitly set to its default is still the operator's value.
func TestOperatorSetPathsCountsAValueEqualToTheDefault(t *testing.T) {
	got, err := OperatorSetPaths([]byte("collector:\n  interval_seconds: 60\n"), "")
	if err != nil || !got["collector.interval_seconds"] {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestOperatorSetPathsFleetAliases(t *testing.T) {
	raw := []byte(`
mode: fleet
defaults:
  collector_interval_seconds: 90
databases:
  - name: orders
    host: db1
    collector_interval_seconds: 30
  - name: billing
    host: db2
    analyzer_interval_seconds: 300
`)
	orders, err := OperatorSetPaths(raw, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if !orders["collector.interval_seconds"] || orders["analyzer.interval_seconds"] {
		t.Fatalf("orders: %v", sortedKeys(orders))
	}
	billing, err := OperatorSetPaths(raw, "billing")
	if err != nil {
		t.Fatal(err)
	}
	// billing inherits defaults.collector_interval_seconds and sets its own
	// analyzer interval.
	if !billing["collector.interval_seconds"] || !billing["analyzer.interval_seconds"] {
		t.Fatalf("billing: %v", sortedKeys(billing))
	}
	other, err := OperatorSetPaths(raw, "inventory")
	if err != nil {
		t.Fatal(err)
	}
	if !other["collector.interval_seconds"] || other["analyzer.interval_seconds"] {
		t.Fatalf("a database not in the list inherits only defaults: %v", sortedKeys(other))
	}
}

func TestOperatorSetPathsPerDatabaseZeroIsNotSet(t *testing.T) {
	raw := []byte("databases:\n  - name: orders\n    host: h\n    collector_interval_seconds: 0\n")
	got, err := OperatorSetPaths(raw, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if got["collector.interval_seconds"] {
		t.Fatal("a zero per-database interval (falls back) counted as operator-set")
	}
}

func TestOperatorSetPathsMalformedYAML(t *testing.T) {
	for _, raw := range []string{"collector: [\n", "\tcollector: 1\n", "a: b: c\n"} {
		if _, err := OperatorSetPaths([]byte(raw), ""); err == nil {
			t.Errorf("malformed YAML %q accepted", raw)
		}
	}
}

func TestOperatorSetPathsTopLevelNotAMapping(t *testing.T) {
	if _, err := OperatorSetPaths([]byte("- a\n- b\n"), ""); err == nil {
		t.Fatal("a YAML list document was accepted as a config")
	}
}

func TestOperatorSetPathsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("safety:\n  query_timeout_ms: 700\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := OperatorSetPathsFromFile(path, "")
	if err != nil || !got["safety.query_timeout_ms"] {
		t.Fatalf("got %v, %v", got, err)
	}
	none, err := OperatorSetPathsFromFile("", "")
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no config file: %v, %v (want an empty, non-nil set)", none, err)
	}
	_, err = OperatorSetPathsFromFile(filepath.Join(dir, "missing.yaml"), "")
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v, want a wrapped ErrNotExist", err)
	}
}
