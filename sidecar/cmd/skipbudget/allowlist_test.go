package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/skipbudget"
)

// The repository's own allow-list and CI workflow, read from the checkout.
const (
	repoAllowlist = "../../.skip-allowlist"
	repoWorkflow  = "../../../.github/workflows/ci.yml"
)

func repoRules(t *testing.T) []skipbudget.Rule {
	t.Helper()
	file, err := os.Open(repoAllowlist)
	if err != nil {
		t.Fatalf("open the repository allow-list: %v", err)
	}
	defer func() { _ = file.Close() }()
	rules, err := skipbudget.ParseRules(file)
	if err != nil {
		t.Fatalf("parse the repository allow-list: %v", err)
	}
	if len(rules) < 40 {
		t.Fatalf("repository allow-list has %d rules; expected the full list", len(rules))
	}
	return rules
}

// A rule whose reason says a feature needs a newer major ("PG16+") must carry
// a pg qualifier, or it also allows the skip on the majors that have it.
func TestRepoAllowlistQualifiesVersionGatedRules(t *testing.T) {
	versionClaim := regexp.MustCompile(`\bPG1[0-9]\+`)
	gated := 0
	for _, rule := range repoRules(t) {
		if rule.PGMin > 0 || rule.PGMax > 0 {
			gated++
			continue
		}
		if versionClaim.MatchString(rule.Reason) {
			t.Errorf("line %d (%s) claims %q but has no pg qualifier", rule.Line,
				rule.Text, versionClaim.FindString(rule.Reason))
		}
	}
	if gated < 25 {
		t.Errorf("only %d pg-qualified rules; the version-gated block was not qualified", gated)
	}
}

func TestRepoAllowlistPinsKnownVersionBounds(t *testing.T) {
	want := map[string][2]int{ // test name -> {PGMin, PGMax}
		"TestCollectIO":                                  {0, 15},
		"TestEnsure_RefusedBeforePG16":                   {16, 0},
		"TestHint_HintTableIntegration":                  {0, 16},
		"TestAP03_SecurityInvokerViewIsNotExposure":      {0, 14},
		"TestCollectQueries_BlockReadTimeFromStatements": {16, 16},
	}
	found := map[string]bool{}
	for _, rule := range repoRules(t) {
		for name, bounds := range want {
			if !rule.Test.MatchString(name) || found[name] {
				continue
			}
			found[name] = true
			if rule.PGMin != bounds[0] || rule.PGMax != bounds[1] {
				t.Errorf("%s: line %d bounds = [%d,%d], want %v", name, rule.Line,
					rule.PGMin, rule.PGMax, bounds)
			}
		}
	}
	for name := range want {
		if !found[name] {
			t.Errorf("no allow-list rule covers %s", name)
		}
	}
}

// The internal/advisor E2E rule allowed skips of a suite CI never builds.
func TestRepoAllowlistHasNoNeverRunAdvisorE2ERule(t *testing.T) {
	for _, rule := range repoRules(t) {
		if rule.Package.MatchString("internal/advisor") &&
			rule.Test.MatchString("TestBloatE2E_FullPipeline") {
			t.Errorf("line %d still allows the never-run advisor E2E suite", rule.Line)
		}
	}
}

// workflowSteps splits ci.yml into its "- name:" step blocks.
func workflowSteps(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(repoWorkflow)
	if err != nil {
		t.Fatalf("read the CI workflow: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return regexp.MustCompile(`\n\s+- name: `).Split(text, -1)
}

// Every skip-budget call names the server major and runs under bash
// -eo pipefail (shell: bash), so a missing -json file fails the step.
func TestWorkflowSkipBudgetStepsAreStrict(t *testing.T) {
	calls := 0
	for _, step := range workflowSteps(t) {
		if !strings.Contains(step, "cmd/skipbudget") {
			continue
		}
		name, _, _ := strings.Cut(step, "\n")
		if !regexp.MustCompile(`\n\s+shell: bash\n`).MatchString(step) {
			t.Errorf("step %q runs the skip budget without shell: bash (pipefail)", name)
		}
		for _, call := range regexp.MustCompile(`go run \./cmd/skipbudget[^\n]*`).
			FindAllString(step, -1) {
			calls++
			if !strings.Contains(call, " -pg ") {
				t.Errorf("step %q: %q does not pass -pg", name, call)
			}
		}
	}
	// One call per gated job since ci-03b (each bench shard is a leg of one
	// matrix step, not a step of its own): test, integration-matrix,
	// perfgate, bench, safety-bench and agent-servers.
	if calls < 6 {
		t.Fatalf("found %d skip-budget calls in ci.yml, want at least 6", calls)
	}
}

// Selective steps (-run) must prove their tests ran: a renamed test makes
// go test print "no tests to run" and exit 0.
func TestWorkflowSelectiveStepsRequireTheirTests(t *testing.T) {
	// agent-servers checks both of its -run steps in its own skip budget step.
	want := map[string][]string{
		"Skip budget (no test may skip on its own servers)": {"TestKill_", "TestRelease_",
			"TestUnfreeze_", "TestRealPGAuditCorrelation", "TestActivityReports"},
		// ci-03b: one bench step, run by every leg of the pg x shard matrix.
		"PGIncidentBench (${{ matrix.shard.name }})": {"sre-bench TestPGIncidentBench"},
		"AgentSafetyBench\n":                         {"safety-bench TestAgentSafetyBench"},
		"Performance gate\n":                         {"TestPerfGate", "TestPerfGateFirstLook"},
	}
	seen := map[string]int{}
	for _, step := range workflowSteps(t) {
		for prefix, tests := range want {
			if !strings.HasPrefix(step, prefix) {
				continue
			}
			seen[prefix]++
			for _, test := range tests {
				if !regexp.MustCompile(`-require '[^']*` + regexp.QuoteMeta(test)).
					MatchString(step) {
					t.Errorf("step %q does not -require %s", prefix, test)
				}
			}
		}
	}
	for prefix := range want {
		if seen[prefix] == 0 {
			t.Errorf("no ci.yml step starts with %q", prefix)
		}
	}
}

// alerting-12: the alerting and notify database tests must pass in CI, not
// merely avoid skipping. The specs name database-backed tests: a bare
// 'internal/notify .*' is met by any pure unit test even if every DB test is gone.
func TestWorkflowSuiteSkipBudgetsRequireAlertingAndNotify(t *testing.T) {
	steps := 0
	for _, step := range workflowSteps(t) {
		if !strings.HasPrefix(step, "Skip budget\n") {
			continue
		}
		steps++
		for _, spec := range []string{"internal/alerting TestEvaluate_",
			"internal/notify TestPhase2_(ProcessRule|LoadChannel|LoadMatchingRules)_"} {
			if !strings.Contains(step, "-require '"+spec) {
				t.Errorf("a suite skip budget step does not -require %q", spec)
			}
		}
	}
	if steps != 2 {
		t.Fatalf("found %d suite \"Skip budget\" steps, want 2 (test, integration-matrix)", steps)
	}
}
