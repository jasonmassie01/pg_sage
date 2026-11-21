package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cli-01b: EffectiveYAML is `config export` and the Settings YAML
// snippet. No concurrent access tests: it reads one config it is handed
// and keeps no state.

const fixtureSecret = "fixture-secret-7Qz9xK2m"

// Happy path: the keys come out nested as a file nests them, and the
// YAML loads back to the same values.
func TestEffectiveYAML_RoundTripsThroughLoad(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Collector.IntervalSeconds = 45
	cfg.Retention.SnapshotsDays = 21
	cfg.Analyzer.DisabledRules = []string{"seq_scan_heavy", "unused_index"}
	keys := []string{"collector.interval_seconds", "retention.snapshots_days",
		"analyzer.disabled_rules"}
	out, err := EffectiveYAML(cfg, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "collector:\n    interval_seconds: 45\nretention:\n    snapshots_days: 21\n"
	if !strings.HasPrefix(string(out), want) {
		t.Fatalf("yaml:\n%s\nwant it to start with:\n%s", out, want)
	}
	check, err := CheckFile(writeConfigFile(t, string(out)))
	if err != nil || len(check.Problems) != 0 {
		t.Fatalf("exported yaml does not validate: %v %v\n%s", err, check.Problems, out)
	}
	got := check.Config
	if got.Collector.IntervalSeconds != 45 || got.Retention.SnapshotsDays != 21 ||
		strings.Join(got.Analyzer.DisabledRules, ",") != "seq_scan_heavy,unused_index" {
		t.Fatalf("loaded back %d %d %v", got.Collector.IntervalSeconds,
			got.Retention.SnapshotsDays, got.Analyzer.DisabledRules)
	}
}

// Nil/empty: no keys is {} and a nil config is an error.
func TestEffectiveYAML_EmptyAndNil(t *testing.T) {
	out, err := EffectiveYAML(DefaultConfig(), nil, nil)
	if err != nil || string(out) != "{}\n" {
		t.Fatalf("no keys: %q %v; want {}", out, err)
	}
	if _, err := EffectiveYAML(nil, []string{"collector.batch_size"}, nil); err == nil ||
		!strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil config: err %v", err)
	}
}

// Invalid input: a key that is not in the config is refused by name, and
// a whole section holding a secret is never exported.
func TestEffectiveYAML_RefusesUnknownKeysAndSecretSections(t *testing.T) {
	for _, key := range []string{"collector.nope", "nope", "collector.batch_size.deeper"} {
		if _, err := EffectiveYAML(DefaultConfig(), []string{key}, nil); err == nil ||
			!strings.Contains(err.Error(), key) {
			t.Fatalf("%s: err %v; want it named", key, err)
		}
	}
	cfg := DefaultConfig()
	cfg.LLM.APIKey = fixtureSecret
	out, err := EffectiveYAML(cfg, []string{"llm"}, nil)
	if err == nil || strings.Contains(string(out), fixtureSecret) {
		t.Fatalf("section export: err %v, out %q; want a refusal", err, out)
	}
}

// Boundary: a secret is a placeholder whether the caller marks it or only
// its name says so, and whether or not it is set.
func TestEffectiveYAML_SecretsArePlaceholders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.APIKey = fixtureSecret
	cfg.Alerting.SlackWebhookURL = "https://hooks.example/" + fixtureSecret
	cfg.Clone.DLEToken = fixtureSecret
	marked := func(key string) bool { return key == "alerting.slack_webhook_url" }
	keys := []string{"llm.api_key", "alerting.slack_webhook_url", "clone.dle_token",
		"alerting.pagerduty_routing_key"}
	out, err := EffectiveYAML(cfg, keys, marked)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, fixtureSecret) || strings.Contains(text, "7Qz9") {
		t.Fatalf("a secret leaked:\n%s", text)
	}
	for _, want := range []string{"api_key: ${SAGE_LLM_API_KEY}",
		"slack_webhook_url: ${SAGE_ALERTING_SLACK_WEBHOOK_URL}",
		"dle_token: ${SAGE_CLONE_DLE_TOKEN}",
		"pagerduty_routing_key: ${SAGE_ALERTING_PAGERDUTY_ROUTING_KEY}"} {
		if !strings.Contains(text, want) {
			t.Fatalf("yaml:\n%s\nwant %q", text, want)
		}
	}
	t.Setenv("SAGE_LLM_API_KEY", "from-env")
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := CheckFile(path)
	if err != nil || len(check.Problems) != 0 || check.Config.LLM.APIKey != "from-env" {
		t.Fatalf("placeholder did not load from the environment: %v %v", err, check.Problems)
	}
}

func TestSecretPlaceholder(t *testing.T) {
	cases := map[string]string{
		"llm.api_key":                "${SAGE_LLM_API_KEY}",
		"clone.dle_token":            "${SAGE_CLONE_DLE_TOKEN}",
		"briefing.slack_webhook_url": "${SAGE_BRIEFING_SLACK_WEBHOOK_URL}",
	}
	for key, want := range cases {
		if got := SecretPlaceholder(key); got != want {
			t.Fatalf("SecretPlaceholder(%q) = %q; want %q", key, got, want)
		}
	}
	for _, key := range []string{"llm.api_key", "postgres.password", "meta_db_dsn",
		"x.encryption_key", "alerting.slack_webhook_url"} {
		if !LooksSecretKey(key) {
			t.Fatalf("%s does not look secret", key)
		}
	}
	if LooksSecretKey("collector.batch_size") || LooksSecretKey("llm.token_budget_daily") {
		t.Fatal("a plain key looks secret")
	}
}
