package store

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-32 pins the behaviour of the split store files: the read model,
// the validators and the CAS argument checks must read the same after the
// functions move between files.

var updateReadModelGolden = flag.Bool("update-read-model-golden", false,
	"rewrite testdata/config_read_model_golden.json from the current code")

const readModelGoldenPath = "testdata/config_read_model_golden.json"

// readModelGoldenInput is the two configs the golden pins: the shipped
// defaults, and the defaults with every masked secret set.
func readModelGoldenInput() map[string]map[string]any {
	withSecrets := config.DefaultConfig()
	withSecrets.LLM.APIKey = "sk-test-0123456789abcd"
	withSecrets.Alerting.SlackWebhookURL = "https://hooks.example.test/T0/B0/wxyz"
	withSecrets.Alerting.PagerDutyRoutingKey = "pd"
	withSecrets.Clone.DLEToken = "dle-token-9876"
	return map[string]map[string]any{
		"default":      ConfigReadModel(config.DefaultConfig()),
		"with_secrets": ConfigReadModel(withSecrets),
	}
}

func TestConfigReadModel_GoldenIsUnchanged(t *testing.T) {
	got, err := json.MarshalIndent(readModelGoldenInput(), "", "  ")
	if err != nil {
		t.Fatalf("marshal read model: %v", err)
	}
	got = append(got, '\n')
	if *updateReadModelGolden {
		if err := os.MkdirAll(filepath.Dir(readModelGoldenPath), 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(readModelGoldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(readModelGoldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", readModelGoldenPath, err)
	}
	if string(normalizeNewlines(want)) != string(got) {
		t.Fatalf("read model differs from %s; rerun with "+
			"-update-read-model-golden only for an intended change", readModelGoldenPath)
	}
}

func normalizeNewlines(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c != '\r' {
			out = append(out, c)
		}
	}
	return out
}

func TestConfigReadModel_MasksEverySecret(t *testing.T) {
	m := readModelGoldenInput()["with_secrets"]
	cases := map[string]string{
		"llm.api_key":                    strings.Repeat("*", 18) + "abcd",
		"alerting.slack_webhook_url":     strings.Repeat("*", 33) + "wxyz",
		"alerting.pagerduty_routing_key": "**",
		"clone.dle_token":                strings.Repeat("*", 10) + "9876",
	}
	for key, want := range cases {
		field, ok := m[key].(map[string]any)
		if !ok {
			t.Fatalf("%s missing from the read model", key)
		}
		if field["value"] != want || field["source"] != SourceFileDefault {
			t.Errorf("%s = %v (%v), want %q (%s)",
				key, field["value"], field["source"], want, SourceFileDefault)
		}
	}
}

func TestConfigToMap_ZeroValueSections(t *testing.T) {
	zero := configToMap(&config.Config{})
	defaults := configToMap(config.DefaultConfig())
	if len(zero) != len(defaults) || len(zero) == 0 {
		t.Fatalf("zero config has %d keys, defaults %d", len(zero), len(defaults))
	}
	want := map[string]any{
		"collector.interval_seconds":       0,
		"trust.level":                      "",
		"llm.api_key":                      "",
		"analyzer.disabled_rules":          "",
		"logwatch.exclude_applications":    "",
		"analyzer.cache_hit_ratio_warning": float64(0),
		"llm.enabled":                      false,
	}
	for key, value := range want {
		field := zero[key].(map[string]any)
		if field["value"] != value || field["source"] != SourceFileDefault {
			t.Errorf("%s = %#v (%v), want %#v", key, field["value"], field["source"], value)
		}
	}
}

// nullIfEmpty("") is pinned by TestNullIfEmpty in config_store_test.go.

func TestValidateConfigOverride_InvalidInputPinsErrors(t *testing.T) {
	cases := []struct{ key, value, want string }{
		{"nope.key", "1", `validate: unknown config key "nope.key"`},
		{"collector.interval_seconds", "ten",
			`validate: collector.interval_seconds must be an integer, got "ten"`},
		{"analyzer.wraparound_freeze_share", "most",
			`validate: analyzer.wraparound_freeze_share must be a number, got "most"`},
		{"llm.enabled", "yes please",
			`validate: llm.enabled must be true or false, got "yes please"`},
		{"collector.interval_seconds", "4",
			"validate: collector.interval_seconds must be 5-" +
				maxText(t, "collector.interval_seconds") + ", got 4"},
		{"analyzer.wraparound_freeze_share", "1.01",
			"validate: analyzer.wraparound_freeze_share must be 0.01-1, got 1.01"},
		{"trust.level", "reckless", `validate: trust.level must be one of ` +
			`observation, advisory, autonomous, got "reckless"`},
	}
	for _, tc := range cases {
		err := ValidateConfigOverride(tc.key, tc.value)
		if err == nil || err.Error() != tc.want {
			t.Errorf("ValidateConfigOverride(%q, %q) = %v, want %q",
				tc.key, tc.value, err, tc.want)
		}
	}
	err := ValidateConfigOverride("trust.maintenance_window", "someday")
	if err == nil || len(err.Error()) < len("validate: invalid value: ") ||
		err.Error()[:len("validate: invalid value: ")] != "validate: invalid value: " {
		t.Errorf("maintenance window someday = %v, want a validate: invalid value error", err)
	}
	if err := ValidateConfigOverride("trust.maintenance_window", "weeknights"); err != nil {
		t.Errorf("maintenance window weeknights refused: %v", err)
	}
}

func maxText(t *testing.T, key string) string {
	t.Helper()
	spec, ok := config.LookupKey(key)
	if !ok || !spec.HasRange {
		t.Fatalf("%s has no registry range", key)
	}
	return strconv.FormatInt(int64(spec.Max), 10)
}

func TestValidateNumber_Boundaries(t *testing.T) {
	intSpec, _ := config.LookupKey("collector.interval_seconds")
	if intSpec.Min != 5 {
		t.Fatalf("collector.interval_seconds min = %v, want 5", intSpec.Min)
	}
	intCases := map[string]bool{
		"5": true, strconv.Itoa(int(intSpec.Max)): true,
		"4": false, strconv.Itoa(int(intSpec.Max) + 1): false,
	}
	for value, ok := range intCases {
		err := ValidateConfigOverride("collector.interval_seconds", value)
		if (err == nil) != ok {
			t.Errorf("collector.interval_seconds=%s: err=%v, want accepted=%v", value, err, ok)
		}
	}
	floatCases := map[string]bool{"0.01": true, "1": true, "0.009": false, "1.001": false}
	for value, ok := range floatCases {
		err := ValidateConfigOverride("analyzer.wraparound_freeze_share", value)
		if (err == nil) != ok {
			t.Errorf("wraparound_freeze_share=%s: err=%v, want accepted=%v", value, err, ok)
		}
	}
}

// A ConfigStore with no pool panics on Begin, so a returned error proves
// the argument checks ran before any transaction was opened.
func TestSetDatabaseOverridesCAS_RefusesBeforeTransaction(t *testing.T) {
	s := &ConfigStore{}
	unknown := []ConfigOverrideWrite{{Key: "nope.key", Value: "1"}}
	badValue := []ConfigOverrideWrite{{Key: "collector.interval_seconds", Value: "4"}}
	cases := []struct {
		name     string
		writes   []ConfigOverrideWrite
		db       int
		expected uint64
		want     string
	}{
		{"database id zero", nil, 0, 1, "database id must be positive"},
		{"database id zero beats key", unknown, 0, 0, "database id must be positive"},
		{"expected generation zero", nil, 7, 0, "expected generation must be positive"},
		{"unknown key beats generation", unknown, 7, 0,
			`validate: unknown config key "nope.key"`},
		{"bad value beats generation", badValue, 7, 0, "validate: " +
			"collector.interval_seconds must be 5-" +
			maxText(t, "collector.interval_seconds") + ", got 4"},
	}
	for _, tc := range cases {
		gen, err := s.SetDatabaseOverridesCAS(
			context.Background(), tc.writes, tc.db, 1, tc.expected, nil)
		if err == nil || err.Error() != tc.want || gen != 0 {
			t.Errorf("%s: (%d, %v), want (0, %q)", tc.name, gen, err, tc.want)
		}
	}
}

func TestListLedgerByFindingIDs_EmptySendsNoQuery(t *testing.T) {
	// A nil pool panics on Query: an empty input must never reach it.
	s := &ActionStore{}
	for _, ids := range [][]int{nil, {}} {
		got, err := s.ListLedgerByFindingIDs(context.Background(), ids)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("ListLedgerByFindingIDs(%v) = (%v, %v), want empty map",
				ids, got, err)
		}
	}
}
