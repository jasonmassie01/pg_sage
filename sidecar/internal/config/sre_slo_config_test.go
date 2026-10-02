package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sage SRE M5 config (sre.slo.*, sre.change_events.*): SLO evaluation,
// the database proxy SLIs and the change feed are on by default (safe,
// read-only, no external setup); the Prometheus connector, pushed SLIs
// and signed change events need explicit configuration. Absent files,
// partial sections and explicit zeros cannot mask the defaults.

const secret32 = "0123456789abcdef0123456789abcdef"

func TestSRESLODefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.SRE.SLO
	if !s.Enabled || s.EvaluationIntervalSeconds != 60 || !s.OpenInvestigations ||
		len(s.BurnRules) != 0 || s.Prometheus.URL != "" || s.Prometheus.TimeoutSeconds != 5 ||
		s.Push.HMACSecret != "" || s.Push.TimestampToleranceSeconds != 300 ||
		len(s.Objectives) != 0 {
		t.Fatalf("slo defaults = %+v", s)
	}
	p := s.Proxies
	if !p.Enabled || p.Target != 0.99 || p.WindowDays != 30 || p.LatencyThresholdMs != 0 ||
		p.LatencyFactor != 3 || p.LatencyFloorMs != 50 || p.TopQueries != 20 ||
		p.ReplicationLagBudgetSeconds != 60 {
		t.Fatalf("proxy defaults = %+v", p)
	}
	c := cfg.SRE.ChangeEvents
	if c.HMACSecret != "" || c.TimestampToleranceSeconds != 300 || !c.FeedEnabled ||
		c.FeedIntervalSeconds != 60 || c.RetentionDays != 90 || len(c.AllowedSources) != 0 {
		t.Fatalf("change event defaults = %+v", c)
	}
	if s.EvaluationInterval() != time.Minute || c.FeedInterval() != time.Minute ||
		c.Tolerance() != 5*time.Minute || c.Retention() != 90*24*time.Hour {
		t.Fatal("duration helpers disagree with the defaults")
	}
}

func TestSRESLOConfig_PartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  slo:\n    proxies:\n      target: 0.995\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SRE.SLO.Proxies.Target != 0.995 || cfg.SRE.SLO.Proxies.TopQueries != 20 ||
		!cfg.SRE.SLO.Enabled || cfg.SRE.SLO.EvaluationIntervalSeconds != 60 {
		t.Fatalf("slo = %+v", cfg.SRE.SLO)
	}
}

// fullSLOYAML configures every M5 setting; secrets come from ${ENV}.
const fullSLOYAML = `sre:
  slo:
    burn_rules:
      - {severity: page, long_window: 2h, short_window: 10m, factor: 10}
      - {severity: ticket, long_window: 3d, short_window: 6h, factor: 1}
    prometheus:
      url: https://prometheus.example:9090
      bearer_token: ${PROM_TOKEN}
      timeout_seconds: 10
    push:
      hmac_secret: ${SLI_SECRET}
    objectives:
      - name: checkout-availability
        database: orders
        owner: team-checkout
        source: prometheus
        target: 0.999
        window_days: 28
        bad_query: 'sum(increase(http_requests_total{code=~"5.."}[$window])) or vector(0)'
        eligible_query: 'sum(increase(http_requests_total[$window]))'
      - name: payments
        source: push
        target: 0.995
  change_events:
    hmac_secret: ${SLI_SECRET}
    allowed_sources: [github-actions, argo]
`

func TestSRESLOConfig_FullObjectives(t *testing.T) {
	t.Setenv("PROM_TOKEN", "tok-xyz")
	t.Setenv("SLI_SECRET", secret32)
	body := fullSLOYAML
	cfg, err := loadRCAYAML(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.SRE.SLO
	if s.Prometheus.BearerToken != "tok-xyz" || s.Push.HMACSecret != secret32 ||
		len(s.Objectives) != 2 || len(s.BurnRules) != 2 {
		t.Fatalf("slo = %+v", s)
	}
	rules, err := s.Rules()
	if err != nil || len(rules) != 2 || rules[0].Long != 2*time.Hour ||
		rules[0].Short != 10*time.Minute || rules[1].Long != 72*time.Hour {
		t.Fatalf("rules = %+v err=%v", rules, err)
	}
	o := s.Objectives[1]
	if o.Window() != 30*24*time.Hour || o.MinEligible() != 50 ||
		o.StaleAfter() != 5*time.Minute {
		t.Fatalf("objective defaults for zero values: %v %v %v", o.Window(),
			o.MinEligible(), o.StaleAfter())
	}
	if s.Objectives[0].Window() != 28*24*time.Hour {
		t.Fatalf("explicit window = %v", s.Objectives[0].Window())
	}
	if len(cfg.SRE.ChangeEvents.AllowedSources) != 2 {
		t.Fatalf("change events = %+v", cfg.SRE.ChangeEvents)
	}
}

// requireInvalid loads each section under prefix and requires an error
// naming key that does not leak a secret.
func requireInvalid(t *testing.T, prefix, key string, cases map[string]string) {
	t.Helper()
	for name, section := range cases {
		t.Run(name, func(t *testing.T) {
			yaml := prefix + section
			_, err := loadRCAYAML(t, yaml)
			if err == nil {
				t.Fatalf("accepted:\n%s", yaml)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error must name the %s key: %v", key, err)
			}
			for _, s := range []string{"short-secret", "tooshort"} {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error leaks the secret: %v", err)
				}
			}
		})
	}
}

func rule(sev, long, short, factor string) string {
	return "    burn_rules: [{severity: " + sev + ", long_window: " + long +
		", short_window: " + short + ", factor: " + factor + "}]\n"
}

const (
	promSection = "    prometheus:\n      url: http://prom:9090\n"
	pushSection = "    push:\n      hmac_secret: " + secret32 + "\n"
	pushA       = "name: a\nsource: push\ntarget: 0.99"
	promQueries = "\nbad_query: 'x[$window]'\neligible_query: 'y[$window]'"
)

// objective renders one objectives entry from "key: value" lines.
func objective(fields string) string {
	return "    objectives:\n      - " + strings.ReplaceAll(fields, "\n", "\n        ") + "\n"
}

func TestSRESLOConfig_InvalidSettings(t *testing.T) {
	requireInvalid(t, "sre:\n  slo:\n", "sre.slo.", map[string]string{
		"interval low":      "    evaluation_interval_seconds: 14\n",
		"interval zero":     "    evaluation_interval_seconds: 0\n",
		"rule severity":     rule("warn", "1h", "5m", "2"),
		"rule windows":      rule("page", "5m", "1h", "2"),
		"rule bad duration": rule("page", "soon", "5m", "2"),
		"rule factor":       rule("page", "1h", "5m", "0"),
		"prom scheme":       "    prometheus:\n      url: prom:9090\n",
		"prom credentials":  "    prometheus:\n      url: http://u:p@prom:9090\n",
		"prom timeout":      promSection + "      timeout_seconds: 61\n",
		"prom two tokens":   promSection + "      bearer_token: a\n      bearer_token_file: /x\n",
		"short push secret": "    push:\n      hmac_secret: short-secret\n",
		"push tolerance":    "    push:\n      timestamp_tolerance_seconds: 29\n",
		"proxy target":      "    proxies:\n      target: 1\n",
		"proxy window":      "    proxies:\n      window_days: 91\n",
		"proxy factor":      "    proxies:\n      latency_factor: 0.5\n",
		"proxy top":         "    proxies:\n      top_queries: 0\n",
		"proxy lag":         "    proxies:\n      replication_lag_budget_seconds: 0\n",
	})
}

func TestSRESLOConfig_InvalidObjectives(t *testing.T) {
	requireInvalid(t, "sre:\n  slo:\n", "sre.slo.", map[string]string{
		"no name":       pushSection + objective("source: push\ntarget: 0.99"),
		"bad name":      pushSection + objective("name: Check Out\nsource: push\ntarget: 0.99"),
		"reserved name": pushSection + objective("name: db_latency\nsource: push\ntarget: 0.99"),
		"duplicate names": pushSection + objective(pushA) +
			"      - {name: a, source: push, target: 0.99}\n",
		"bad source":      objective("name: a\nsource: statsd\ntarget: 0.99"),
		"target 1":        pushSection + objective("name: a\nsource: push\ntarget: 1"),
		"window 91":       pushSection + objective(pushA+"\nwindow_days: 91"),
		"push no secret":  objective(pushA),
		"prom no url":     objective("name: a\nsource: prometheus\ntarget: 0.99" + promQueries),
		"prom no $window": promSection + objective("name: a\nsource: prometheus\ntarget: 0.99\n"+
			"bad_query: x\neligible_query: 'y[$window]'"),
		"push with query": pushSection + objective(pushA+"\nbad_query: 'x[$window]'"),
		"negative min":    pushSection + objective(pushA+"\nmin_eligible_events: -1"),
	})
}

func TestSREChangeEventsConfig_Invalid(t *testing.T) {
	requireInvalid(t, "sre:\n  change_events:\n", "sre.change_events.", map[string]string{
		"short secret":  "    hmac_secret: tooshort\n",
		"tolerance":     "    timestamp_tolerance_seconds: 3601\n",
		"bad source":    "    allowed_sources: [GitHub Actions]\n",
		"feed interval": "    feed_interval_seconds: 5\n",
		"retention":     "    retention_days: 0\n",
	})
}

// The bearer token file is read at use time; a missing file is an error
// that names the file, not the token.
func TestSREPrometheusConfig_TokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := SREPrometheusConfig{URL: "http://prom:9090", BearerTokenFile: path}
	tok, err := p.Token()
	if err != nil || tok != "file-token" {
		t.Fatalf("token = %q err=%v", tok, err)
	}
	p.BearerTokenFile = filepath.Join(dir, "missing")
	if _, err := p.Token(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing file: err = %v", err)
	}
	inline := SREPrometheusConfig{BearerToken: "inline"}
	if tok, err := inline.Token(); err != nil || tok != "inline" {
		t.Fatalf("inline token = %q err=%v", tok, err)
	}
}

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{"5m": 5 * time.Minute, "1h": time.Hour,
		"3d": 72 * time.Hour, "30d": 720 * time.Hour, "90s": 90 * time.Second} {
		got, err := ParseWindow(in)
		if err != nil || got != want {
			t.Errorf("ParseWindow(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "-1h", "0m", "3x", "1.5d"} {
		if _, err := ParseWindow(in); err == nil {
			t.Errorf("ParseWindow(%q) accepted", in)
		}
	}
}

// Clone deep-copies the SLO and change-event lists: a reload candidate
// can never alias the running configuration.
func TestSRESLOConfig_CloneIsDeep(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SRE.SLO.BurnRules = []SREBurnRuleConfig{{Severity: "page", LongWindow: "1h",
		ShortWindow: "5m", Factor: 14.4}}
	cfg.SRE.SLO.Objectives = []SREObjectiveConfig{{Name: "checkout", Source: "push",
		Target: 0.999}}
	cfg.SRE.ChangeEvents.AllowedSources = []string{"ci"}
	cp := Clone(cfg)
	cp.SRE.SLO.BurnRules[0].Factor = 1
	cp.SRE.SLO.Objectives[0].Name = "changed"
	cp.SRE.ChangeEvents.AllowedSources[0] = "other"
	if cfg.SRE.SLO.BurnRules[0].Factor != 14.4 || cfg.SRE.SLO.Objectives[0].Name != "checkout" ||
		cfg.SRE.ChangeEvents.AllowedSources[0] != "ci" {
		t.Fatalf("clone aliases the original: %+v", cfg.SRE)
	}
}
