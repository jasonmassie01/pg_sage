package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-09: deploy/prometheus/pg_sage.rules.yml against the
// registry and the runbooks. promtool (CI) evaluates the rules; these
// checks run everywhere go test does.

const runbookBase = "https://jasonmassie01.github.io/pg_sage/metrics/#"

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

type ruleTests struct {
	RuleFiles []string `yaml:"rule_files"`
	Tests     []struct {
		AlertRuleTest []struct {
			Alertname string           `yaml:"alertname"`
			ExpAlerts []map[string]any `yaml:"exp_alerts"`
		} `yaml:"alert_rule_test"`
	} `yaml:"tests"`
}

var exprMetric = regexp.MustCompile(`\bpg_sage_[a-z0-9_]+`)

func loadRules(t *testing.T) ruleFile {
	t.Helper()
	var rf ruleFile
	if err := yaml.Unmarshal([]byte(readRepo(t, "deploy/prometheus/pg_sage.rules.yml")),
		&rf); err != nil {
		t.Fatalf("parse rules: %v", err)
	}
	if len(rf.Groups) != 1 || len(rf.Groups[0].Rules) == 0 {
		t.Fatalf("rules file has %d groups; want one with rules", len(rf.Groups))
	}
	return rf
}

// Every alert has its runbook, in order, with its severity and a
// runbook_url into docs/metrics.md, and reads only registered families.
func TestRulesMatchTheRunbooksAndTheRegistry(t *testing.T) {
	rules := loadRules(t).Groups[0].Rules
	if len(rules) != len(runbooks) {
		t.Fatalf("%d rules, %d runbooks", len(rules), len(runbooks))
	}
	for i, r := range rules {
		book := runbooks[i]
		if r.Alert != book.Alert || r.Labels["severity"] != book.Severity {
			t.Errorf("rule %d = %s/%s, runbook %s/%s", i, r.Alert, r.Labels["severity"],
				book.Alert, book.Severity)
		}
		if want := runbookBase + strings.ToLower(r.Alert); r.Annotations["runbook_url"] != want {
			t.Errorf("%s runbook_url = %q, want %q", r.Alert, r.Annotations["runbook_url"], want)
		}
		if r.Annotations["summary"] == "" {
			t.Errorf("%s has no summary", r.Alert)
		}
		if len(exprMetric.FindAllString(r.Expr, -1)) == 0 && !strings.Contains(r.Expr, "up{") {
			t.Errorf("%s reads no pg_sage family: %s", r.Alert, r.Expr)
		}
		for _, n := range unregisteredFamilies(r.Expr) {
			t.Errorf("%s reads %s, which is not registered", r.Alert, n)
		}
	}
}

// unregisteredFamilies returns the pg_sage_* names in expr that the
// registry does not know, in order of appearance.
func unregisteredFamilies(expr string) []string {
	var out []string
	for _, n := range exprMetric.FindAllString(expr, -1) {
		if _, ok := metricsreg.Lookup(n); !ok {
			out = append(out, n)
		}
	}
	return out
}

// Every alert is exercised by promtool, firing at least once.
func TestRulesTestExercisesEveryAlert(t *testing.T) {
	var rt ruleTests
	if err := yaml.Unmarshal([]byte(readRepo(t,
		"deploy/prometheus/pg_sage.rules_test.yml")), &rt); err != nil {
		t.Fatalf("parse rule tests: %v", err)
	}
	if len(rt.RuleFiles) != 1 || rt.RuleFiles[0] != "pg_sage.rules.yml" {
		t.Fatalf("rule_files = %v, want [pg_sage.rules.yml]", rt.RuleFiles)
	}
	fires := map[string]bool{}
	tested := map[string]bool{}
	for _, tc := range rt.Tests {
		for _, a := range tc.AlertRuleTest {
			tested[a.Alertname] = true
			fires[a.Alertname] = fires[a.Alertname] || len(a.ExpAlerts) > 0
		}
	}
	var untested []string
	for _, r := range loadRules(t).Groups[0].Rules {
		if !fires[r.Alert] {
			untested = append(untested, r.Alert)
		}
		delete(tested, r.Alert)
	}
	sort.Strings(untested)
	if len(untested) > 0 || len(tested) > 0 {
		t.Fatalf("alerts never firing in a test: %v; tests of unknown alerts: %v",
			untested, tested)
	}
}

// Invalid input: an unregistered family in a rule is caught.
func TestExprMetricFindsFamilyNames(t *testing.T) {
	got := exprMetric.FindAllString(`increase(pg_sage_actions_total{x="pg"}[5m]) > 0 or `+
		`up{job="pg_sage"} == 0`, -1)
	if strings.Join(got, ",") != "pg_sage_actions_total" {
		t.Fatalf("names = %v, want pg_sage_actions_total only", got)
	}
	if _, ok := metricsreg.Lookup("pg_sage_executor_actions_total"); ok {
		t.Fatal("pg_sage_executor_actions_total, which never existed, is registered")
	}
}
