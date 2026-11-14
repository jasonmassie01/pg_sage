package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/servertls"
)

// observability-09b: the TLS expiry, leader lease, SSO discovery and
// waiting-approval alerts. The rule files are static, so error
// propagation, concurrent access and database integration do not apply;
// promtool (CI) evaluates the boundaries, and the live firing is an owner
// check.

// existingAlerts are the observability-09 rules, which must not change.
var existingAlerts = []string{"PgSageDown", "PgSageDatabaseDown", "PgSageCollectorStale",
	"PgSageLLMCircuitOpen", "PgSageActionFailed", "PgSageActionRolledBack",
	"PgSageSelfBudgetExceeded", "PgSageSLOPage", "PgSageInvestigationBacklogOld"}

// runtimeAlert is a planned rule: its expression, hold and severity.
type runtimeAlert struct{ alert, expr, hold, severity string }

var runtimeAlerts = []runtimeAlert{
	{"PgSageTLSCertExpiring",
		"pg_sage_api_tls_cert_not_after_seconds - time() < 14 * 86400", "1h", "ticket"},
	{"PgSageLeaderLeaseStale", "pg_sage_leader_last_renew_age_seconds > 90", "2m", "page"},
	{"PgSageSSODiscoveryDown", "pg_sage_sso_discovery_ready == 0", "15m", "ticket"},
	{"PgSageApprovalsWaiting", "pg_sage_actions_pending_approval > 0", "24h", "ticket"},
}

// Happy path: the nine earlier alerts keep their order, and the four new
// ones follow with the planned expression, hold and severity.
func TestRuntimeAlertsFollowTheExistingNine(t *testing.T) {
	rules := loadRules(t).Groups[0].Rules
	if want := len(existingAlerts) + len(runtimeAlerts); len(rules) != want {
		t.Fatalf("%d rules, want %d", len(rules), want)
	}
	for i, name := range existingAlerts {
		if rules[i].Alert != name {
			t.Errorf("rule %d = %s, want %s", i, rules[i].Alert, name)
		}
	}
	for i, want := range runtimeAlerts {
		r := rules[len(existingAlerts)+i]
		got := runtimeAlert{r.Alert, strings.TrimSpace(r.Expr), r.For, r.Labels["severity"]}
		if got != want {
			t.Errorf("rule %d = %+v, want %+v", len(existingAlerts)+i, got, want)
		}
		if names := exprMetric.FindAllString(r.Expr, -1); len(names) != 1 {
			t.Errorf("%s reads %v, want exactly one family", r.Alert, names)
		}
		if bad := unregisteredFamilies(r.Expr); len(bad) != 0 {
			t.Errorf("%s reads unregistered %v", r.Alert, bad)
		}
	}
}

// Invalid input: a rule naming a family that does not exist is reported,
// and only that name is.
func TestUnregisteredFamilyInARuleIsReported(t *testing.T) {
	cases := []struct {
		expr string
		want []string
	}{
		{"pg_sage_sso_ready == 0", []string{"pg_sage_sso_ready"}},
		{"pg_sage_sso_discovery_ready == 0", nil},
		{"pg_sage_leader_last_renew_age_seconds > 90 or pg_sage_leader_lease_age > 1",
			[]string{"pg_sage_leader_lease_age"}},
		{"", nil},
		{`up{job="pg_sage"} == 0`, nil},
	}
	for _, c := range cases {
		got := unregisteredFamilies(c.expr)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("unregisteredFamilies(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// ruleExpr returns the expression of the named alert.
func ruleExpr(t *testing.T, alert string) string {
	t.Helper()
	for _, r := range loadRules(t).Groups[0].Rules {
		if r.Alert == alert {
			return strings.TrimSpace(r.Expr)
		}
	}
	t.Fatalf("no rule %s", alert)
	return ""
}

// Boundary: the stale-lease threshold is three lease TTLs, so one missed
// renewal (every TTL/3) does not page, but a lease that lapsed does.
func TestLeaderRuleThresholdIsThreeTTL(t *testing.T) {
	expr := ruleExpr(t, "PgSageLeaderLeaseStale")
	m := regexp.MustCompile(`^pg_sage_leader_last_renew_age_seconds > (\d+)$`).
		FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("PgSageLeaderLeaseStale expr %q is not `age > N`", expr)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("threshold %q: %v", m[1], err)
	}
	if want := 3 * leader.DefaultTTL; time.Duration(n)*time.Second != want {
		t.Fatalf("threshold = %ds, want 3 x leader.DefaultTTL = %s", n, want)
	}
}

// Boundary: the certificate alert opens the ticket at the same 14 days
// /ready starts reporting tls_expiring.
func TestTLSRuleThresholdIsTheExpiryWarning(t *testing.T) {
	expr := ruleExpr(t, "PgSageTLSCertExpiring")
	m := regexp.MustCompile(
		`^pg_sage_api_tls_cert_not_after_seconds - time\(\) < (\d+) \* 86400$`).
		FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("PgSageTLSCertExpiring expr %q is not `not_after - time() < D * 86400`", expr)
	}
	days, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("days %q: %v", m[1], err)
	}
	if got := time.Duration(days) * 24 * time.Hour; got != servertls.ExpiryWarning {
		t.Fatalf("threshold = %s, want servertls.ExpiryWarning = %s", got,
			servertls.ExpiryWarning)
	}
}

// promtoolCases is the part of pg_sage.rules_test.yml these checks read.
type promtoolCases struct {
	Tests []struct {
		InputSeries []struct {
			Series string `yaml:"series"`
		} `yaml:"input_series"`
		AlertRuleTest []struct {
			Alertname string           `yaml:"alertname"`
			ExpAlerts []map[string]any `yaml:"exp_alerts"`
		} `yaml:"alert_rule_test"`
	} `yaml:"tests"`
}

func loadPromtoolCases(t *testing.T) promtoolCases {
	t.Helper()
	var pc promtoolCases
	if err := yaml.Unmarshal([]byte(readRepo(t,
		"deploy/prometheus/pg_sage.rules_test.yml")), &pc); err != nil {
		t.Fatalf("parse rule tests: %v", err)
	}
	return pc
}

// Boundary and state transition: each new alert has a promtool case where
// it stays silent as well as one where it fires.
func TestRuntimeAlertsHaveSilentAndFiringCases(t *testing.T) {
	silent, fires := map[string]int{}, map[string]int{}
	for _, tc := range loadPromtoolCases(t).Tests {
		for _, a := range tc.AlertRuleTest {
			if len(a.ExpAlerts) == 0 {
				silent[a.Alertname]++
			} else {
				fires[a.Alertname]++
			}
		}
	}
	for _, a := range runtimeAlerts {
		if silent[a.alert] < 2 || fires[a.alert] == 0 {
			t.Errorf("%s: %d silent and %d firing promtool checks, want >= 2 and >= 1",
				a.alert, silent[a.alert], fires[a.alert])
		}
	}
}

// Invalid input: a misspelt series in a promtool case would make its
// silent checks pass for the wrong reason, so every input series is a
// registered family (or Prometheus's own up).
func TestPromtoolInputSeriesAreRegistered(t *testing.T) {
	seen := 0
	for i, tc := range loadPromtoolCases(t).Tests {
		for _, s := range tc.InputSeries {
			name, _, _ := strings.Cut(strings.TrimSpace(s.Series), "{")
			seen++
			if name == "up" {
				continue
			}
			if _, ok := metricsreg.Lookup(name); !ok {
				t.Errorf("test %d feeds %s, which is not registered", i, name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no input series found; the test file did not parse as expected")
	}
}
