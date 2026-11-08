package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// alerting-08: an enabled alerting block is validated at load, so a typo
// fails startup naming the key instead of leaving an operator with no
// pages. A disabled block is not validated.
// No state-transition test beyond enabled/disabled: validation is a pure
// function of one loaded config.

const alertSlackURL = "  slack_webhook_url: https://hooks.slack.example.com/x\n"

func loadAlertingYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return loadRCAYAML(t, "alerting:\n"+body)
}

func requireAlertingErr(t *testing.T, body string, wants ...string) {
	t.Helper()
	cfg, err := loadAlertingYAML(t, body)
	if err == nil {
		t.Fatalf("config loaded (routes %+v), want a refusal naming %q",
			cfg.Alerting.Routes, wants)
	}
	if !strings.Contains(err.Error(), "config validation") {
		t.Errorf("err = %v, want it wrapped as a config validation error", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err = %v, want it to mention %q", err, w)
		}
	}
}

func TestAlertingValidate_ChannelCaseTypoFailsNamingSlack(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL+
		"  routes:\n    - severity: critical\n      channels: [Slack]\n",
		"alerting.routes[0].channels[0]", `"Slack"`, `"slack"`)
}

func TestAlertingValidate_UnknownSeverityFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL+
		"  routes:\n    - severity: high\n      channels: [slack]\n",
		"alerting.routes[0].severity", `"high"`, "critical, warning, info")
}

func TestAlertingValidate_SeverityIsCaseSensitive(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL+
		"  routes:\n    - severity: Critical\n      channels: [slack]\n",
		"alerting.routes[0].severity", `"Critical"`)
}

func TestAlertingValidate_QuietHoursOutOfRangeFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL+
		"  quiet_hours_start: \"25:00\"\n  quiet_hours_end: \"07:00\"\n"+
		"  routes:\n    - severity: critical\n      channels: [slack]\n",
		"alerting.quiet_hours_start", `"25:00"`, "HH:MM")
}

func TestAlertingValidate_QuietHoursBoundaries(t *testing.T) {
	for _, tc := range []struct {
		start, end string
		ok         bool
	}{
		{"00:00", "23:59", true}, {"22:00", "07:00", true}, {"22", "7", true},
		{"24:00", "07:00", false}, {"22:00", "12:60", false},
		{"-1:00", "07:00", false}, {"22:0x", "07:00", false},
		{"22:00", "", false}, {"", "07:00", false}, {"", "", true},
	} {
		body := "  enabled: true\n" + alertSlackURL +
			"  quiet_hours_start: \"" + tc.start + "\"\n" +
			"  quiet_hours_end: \"" + tc.end + "\"\n" +
			"  routes:\n    - severity: critical\n      channels: [slack]\n"
		_, err := loadAlertingYAML(t, body)
		if tc.ok && err != nil {
			t.Errorf("%q-%q refused: %v", tc.start, tc.end, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "alerting.quiet_hours")) {
			t.Errorf("%q-%q: err = %v, want a refusal naming alerting.quiet_hours",
				tc.start, tc.end, err)
		}
	}
}

func TestAlertingValidate_UnknownTimezoneFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL+
		"  timezone: Mars/Olympus_Mons\n"+
		"  routes:\n    - severity: critical\n      channels: [slack]\n",
		"alerting.timezone", `"Mars/Olympus_Mons"`)
}

func TestAlertingValidate_EmptyTimezoneMeansUTC(t *testing.T) {
	cfg, err := loadAlertingYAML(t, "  enabled: true\n"+alertSlackURL+
		"  timezone: \"\"\n  routes:\n    - severity: info\n      channels: [slack]\n")
	if err != nil {
		t.Fatalf("empty timezone refused: %v", err)
	}
	if cfg.Alerting.Timezone != "" || len(cfg.Alerting.Routes) != 1 {
		t.Fatalf("alerting = %+v, want the empty timezone and one route kept", cfg.Alerting)
	}
}

func TestAlertingValidate_BareWebhookNameIsAlias(t *testing.T) {
	cfg, err := loadAlertingYAML(t, "  enabled: true\n"+
		"  webhooks:\n    - name: myapp\n      url: https://hooks.example.com/pg-sage\n"+
		"  routes:\n    - severity: critical\n      channels: [myapp]\n"+
		"    - severity: warning\n      channels: [\"webhook:myapp\"]\n")
	if err != nil {
		t.Fatalf("bare webhook name refused: %v", err)
	}
	for _, name := range []string{"myapp", "webhook:myapp"} {
		key, ok := cfg.Alerting.ResolveAlertChannel(name)
		if !ok || key != "webhook:myapp" {
			t.Errorf("ResolveAlertChannel(%q) = %q, %v; want webhook:myapp, true",
				name, key, ok)
		}
	}
	if !cfg.Alerting.HasUsableRoute() {
		t.Fatal("HasUsableRoute = false for a config with two webhook routes")
	}
}

func TestAlertingValidate_EnabledWithoutRoutesFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL, "alerting.routes", "no alert")
}

func TestAlertingValidate_RouteWithoutChannelsFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL+
		"  routes:\n    - severity: critical\n      channels: []\n",
		"alerting.routes[0].channels")
}

// A route naming slack or pagerduty loads without the URL or key, which a
// persisted dashboard override can still supply (the Settings page offers
// the Slack URL); the effective-config check then refuses it if none came.
func requireSecretDeferredToEffective(t *testing.T, body string, wants ...string) {
	t.Helper()
	cfg, err := loadAlertingYAML(t, body)
	if err != nil {
		t.Fatalf("Load refused a route whose secret an override can supply: %v", err)
	}
	err = cfg.ValidateAlerting()
	if err == nil {
		t.Fatalf("effective check passed routes %+v with no secret", cfg.Alerting.Routes)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err = %v, want it to mention %q", err, w)
		}
	}
}

func TestAlertingValidate_SlackRouteWithoutURLFails(t *testing.T) {
	requireSecretDeferredToEffective(t, "  enabled: true\n"+
		"  routes:\n    - severity: critical\n      channels: [slack]\n",
		"alerting.routes[0].channels[0]", "alerting.slack_webhook_url")
}

func TestAlertingValidate_PagerDutyRouteWithoutKeyFails(t *testing.T) {
	requireSecretDeferredToEffective(t, "  enabled: true\n"+alertSlackURL+
		"  routes:\n    - severity: critical\n      channels: [slack, pagerduty]\n",
		"alerting.routes[0].channels[1]", "alerting.pagerduty_routing_key")
}

// Once the override supplies the secret, the same file passes in full.
func TestAlertingValidate_OverrideSuppliedSecretsPass(t *testing.T) {
	cfg, err := loadAlertingYAML(t, "  enabled: true\n"+
		"  routes:\n    - severity: critical\n      channels: [slack, pagerduty]\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerting.HasUsableRoute() {
		t.Fatal("HasUsableRoute = true before any secret is set")
	}
	cfg.Alerting.SlackWebhookURL = "https://hooks.slack.example.com/x"
	cfg.Alerting.PagerDutyRoutingKey = "fixture-routing-key"
	if err := cfg.ValidateAlerting(); err != nil {
		t.Fatalf("effective check refused override-supplied secrets: %v", err)
	}
	if !cfg.Alerting.HasUsableRoute() {
		t.Fatal("HasUsableRoute = false once the secrets are set")
	}
}

// Deferring secrets must not let an unknown name through at load.
func TestAlertingValidate_DeferredSecretsStillRefuseUnknownNames(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+
		"  routes:\n    - severity: critical\n      channels: [PagerDuty]\n",
		"alerting.routes[0].channels[0]", `"PagerDuty"`, `"pagerduty"`)
}

func TestAlertingValidate_UnknownWebhookRouteFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+
		"  webhooks:\n    - name: myapp\n      url: https://hooks.example.com/a\n"+
		"  routes:\n    - severity: critical\n      channels: [\"webhook:other\"]\n",
		"alerting.routes[0].channels[0]", `"webhook:other"`, "alerting.webhooks")
}

func TestAlertingValidate_DuplicateWebhookNameFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+
		"  webhooks:\n    - name: myapp\n      url: https://hooks.example.com/a\n"+
		"    - name: myapp\n      url: https://hooks.example.com/b\n"+
		"  routes:\n    - severity: critical\n      channels: [myapp]\n",
		"alerting.webhooks[1].name", `"myapp"`, "duplicate")
}

func TestAlertingValidate_EmptyWebhookNameOrURLFails(t *testing.T) {
	requireAlertingErr(t, "  enabled: true\n"+
		"  webhooks:\n    - name: \"\"\n      url: https://hooks.example.com/a\n"+
		"  routes:\n    - severity: critical\n      channels: [\"webhook:\"]\n",
		"alerting.webhooks[0].name")
	requireAlertingErr(t, "  enabled: true\n"+
		"  webhooks:\n    - name: myapp\n      url: \"\"\n"+
		"  routes:\n    - severity: critical\n      channels: [myapp]\n",
		"alerting.webhooks[0].url")
}

func TestAlertingValidate_DisabledBlockIsNotValidated(t *testing.T) {
	cfg, err := loadAlertingYAML(t, "  enabled: false\n"+
		"  quiet_hours_start: \"99:99\"\n  timezone: Nowhere/Land\n"+
		"  webhooks:\n    - name: dup\n      url: \"\"\n    - name: dup\n      url: \"\"\n"+
		"  routes:\n    - severity: high\n      channels: [nope]\n")
	if err != nil {
		t.Fatalf("disabled alerting block refused: %v", err)
	}
	a := cfg.Alerting
	if a.Enabled || a.Timezone != "Nowhere/Land" || len(a.Routes) != 1 ||
		a.Routes[0].Severity != "high" || len(a.Webhooks) != 2 {
		t.Fatalf("disabled block not kept as written: %+v", a)
	}
	if a.HasUsableRoute() {
		t.Fatal("HasUsableRoute = true for a route naming an unknown channel")
	}
}

func TestAlertingValidate_NoConfigFileDefaultsPass(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerting.Enabled || cfg.Alerting.HasUsableRoute() {
		t.Fatalf("default alerting = %+v, want disabled with no usable route", cfg.Alerting)
	}
}

// The shipped example keeps its two routes, and once its secrets are
// filled in and alerting is switched on every route resolves.
func TestAlertingValidate_ShippedExampleLoadsWithTwoRoutes(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("shipped example missing: %v", err)
	}
	chdirTemp(t)
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("example does not load: %v", err)
	}
	r := cfg.Alerting.Routes
	if len(r) != 2 || r[0].Severity != "critical" || r[1].Severity != "warning" {
		t.Fatalf("example routes = %+v, want critical and warning", r)
	}
	cfg.Alerting.Enabled = true
	cfg.Alerting.SlackWebhookURL = "https://hooks.slack.example.com/x"
	cfg.Alerting.PagerDutyRoutingKey = "fixture-routing-key"
	if err := cfg.ValidateAlerting(); err != nil {
		t.Fatalf("example routes do not resolve once enabled: %v", err)
	}
}

func TestResolveAlertChannel_Table(t *testing.T) {
	a := AlertingConfig{
		SlackWebhookURL: "https://hooks.slack.example.com/x",
		Webhooks: []WebhookConfig{
			{Name: "ops", URL: "https://hooks.example.com/ops"},
			{Name: "pagerduty", URL: "https://hooks.example.com/pd"},
			{Name: "nourl"},
		},
	}
	for _, tc := range []struct {
		in, key string
		ok      bool
	}{
		{"slack", "slack", true}, {"Slack", "", false},
		{"pagerduty", "", false}, // built-in wins; no routing key is set
		{"webhook:pagerduty", "webhook:pagerduty", true},
		{"ops", "webhook:ops", true}, {"webhook:ops", "webhook:ops", true},
		{"nourl", "", false}, {"webhook:", "", false}, {"", "", false},
		{"webhook:missing", "", false}, {"missing", "", false},
	} {
		key, ok := a.ResolveAlertChannel(tc.in)
		if key != tc.key || ok != tc.ok {
			t.Errorf("ResolveAlertChannel(%q) = %q, %v; want %q, %v",
				tc.in, key, ok, tc.key, tc.ok)
		}
	}
}

func TestResolveAlertChannel_ZeroValueResolvesNothing(t *testing.T) {
	var a AlertingConfig
	for _, name := range []string{"slack", "pagerduty", "webhook:x", "x", ""} {
		if key, ok := a.ResolveAlertChannel(name); ok || key != "" {
			t.Errorf("zero config resolved %q to %q", name, key)
		}
	}
	if a.HasUsableRoute() {
		t.Fatal("zero config reports a usable route")
	}
}

func TestQuietHourMinute(t *testing.T) {
	for in, want := range map[string]int{
		"": -1, "  ": -1, "7": 420, "07:30": 450, "23:59": 1439, "00:00": 0,
		" 22:00 ": 1320, "24:00": -1, "-1:00": -1, "12:60": -1, "12:5x": -1,
		"ab": -1, "12:-1": -1,
	} {
		if got := QuietHourMinute(in); got != want {
			t.Errorf("QuietHourMinute(%q) = %d, want %d", in, got, want)
		}
	}
}

// Routing and validation read one shared config from many goroutines.
func TestResolveAlertChannel_ConcurrentReaders(t *testing.T) {
	a := AlertingConfig{Enabled: true, PagerDutyRoutingKey: "k",
		Routes: []AlertRoute{{Severity: "critical", Channels: []string{"pagerduty"}}}}
	cfg := &Config{Alerting: a}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if key, ok := cfg.Alerting.ResolveAlertChannel("pagerduty"); !ok ||
				key != "pagerduty" {
				errs <- "resolve " + key
			}
			if err := cfg.ValidateAlerting(); err != nil {
				errs <- err.Error()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// The integration fixtures switch alerting on; each must route to its
// webhook or the sidecar under test refuses to start.
func TestAlertingValidate_IntegrationFixturesRoute(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "tests", "integration"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "config_test*.yaml"))
	if err != nil || len(paths) < 4 {
		t.Fatalf("integration fixtures = %v (err %v), want at least 4", paths, err)
	}
	chdirTemp(t)
	for _, p := range paths {
		cfg, err := Load([]string{"--config", p})
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
			continue
		}
		key, ok := cfg.Alerting.ResolveAlertChannel("test")
		if !cfg.Alerting.Enabled || !ok || key != "webhook:test" ||
			!cfg.Alerting.HasUsableRoute() {
			t.Errorf("%s: alerting %+v does not route to webhook:test", filepath.Base(p),
				cfg.Alerting)
		}
	}
}

// Decision (alerting-08): the alerting check guards the operator's file
// (startup and hot reload), not every API candidate. A persisted
// alerting.enabled override must never block an unrelated settings change
// such as lowering trust; routing logs the unroutable channel instead.
func TestAlertingValidate_CandidateValidationLeavesAlertingToFileLoad(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Alerting.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("API candidate refused over alerting: %v", err)
	}
	err := cfg.validateFileConfig()
	if err == nil || !strings.Contains(err.Error(), "alerting.routes") {
		t.Fatalf("file validation err = %v, want a refusal naming alerting.routes", err)
	}
}

func TestAlertingValidate_WatcherKeepsPreviousOnBrokenAlerting(t *testing.T) {
	broken := DefaultConfig()
	broken.Alerting.Enabled = true
	broken.Alerting.SlackWebhookURL = "https://hooks.slack.example.com/x"
	broken.Alerting.Routes = []AlertRoute{{Severity: "critical", Channels: []string{"Slack"}}}
	calls := 0
	w := NewAcknowledgedWatcherWithLoader(filepath.Join(t.TempDir(), "c.yaml"),
		DefaultConfig(), func() (*Config, error) { return Clone(broken), nil },
		func(*Config) error { calls++; return nil })
	w.reload()
	if calls != 0 || w.Current().Alerting.Enabled {
		t.Fatalf("reload applied broken alerting: onChange calls %d, enabled %v",
			calls, w.Current().Alerting.Enabled)
	}
	broken.Alerting.Routes[0].Channels = []string{"slack"}
	w.reload()
	if calls != 1 || !w.Current().Alerting.Enabled {
		t.Fatalf("reload of fixed alerting: onChange calls %d, enabled %v, want 1, true",
			calls, w.Current().Alerting.Enabled)
	}
}

// The watcher candidate already carries the persisted overrides, so the
// reload applies the full check: a slack route with no URL is refused.
func TestAlertingValidate_WatcherRefusesSecretlessEffectiveCandidate(t *testing.T) {
	candidate := DefaultConfig()
	candidate.Alerting.Enabled = true
	candidate.Alerting.Routes = []AlertRoute{{Severity: "critical", Channels: []string{"slack"}}}
	calls := 0
	w := NewAcknowledgedWatcherWithLoader(filepath.Join(t.TempDir(), "c.yaml"),
		DefaultConfig(), func() (*Config, error) { return Clone(candidate), nil },
		func(*Config) error { calls++; return nil })
	w.reload()
	if calls != 0 || w.Current().Alerting.Enabled {
		t.Fatalf("reload applied a slack route with no URL: onChange calls %d", calls)
	}
}
