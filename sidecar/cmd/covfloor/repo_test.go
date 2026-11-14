package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The repository's floors file, CI workflow and local Linux runner.
const (
	repoFloors   = "../../.coverage-floors"
	repoWorkflow = "../../../.github/workflows/ci.yml"
	repoRunner   = "../../../scripts/ci/local-linux-suite.sh"
)

func repoFloorRules(t *testing.T) []floorRule {
	t.Helper()
	file, err := os.Open(repoFloors)
	if err != nil {
		t.Fatalf("open the repository floors file: %v", err)
	}
	defer func() { _ = file.Close() }()
	rules, err := parseFloors(file)
	if err != nil {
		t.Fatalf("parse the repository floors file: %v", err)
	}
	return rules
}

// CLAUDE.md: 70% for business logic, 50% for helpers. Packages the coverage
// items name (agentgov-12, apiauth-14, ci-21, ci-22, config-14) keep the
// floor they are measured against.
func TestRepoFloorsPinTheIntendedFloors(t *testing.T) {
	rules := repoFloorRules(t)
	want := map[string]float64{
		"internal/testdb":                   50,
		"internal/servertls/servertlstest":  50,
		"internal/testsupport/fakeidp":      50,
		"internal/testsupport/lockqueue":    50,
		"internal/testsupport/snapfixture":  50,
		"internal/testsupport/skipbudget":   50,
		"cmd/gen_config_meta":               50,
		"cmd/gen_finding_catalog":           50,
		"cmd/gen_metrics_doc":               50,
		"cmd/skipbudget":                    50,
		"cmd/covfloor":                      70,
		"cmd/reset_admin_for_test":          70,
		"cmd/create_admin":                  70,
		"cmd/sigstore_trusted_root":         70,
		"cmd/pg_sage_sidecar":               70,
		"internal/agentguard/broker":        70,
		"internal/executor":                 70,
		"internal/sqlast":                   70,
		"e2e":                               0,
		"release-bench":                     0,
		"internal/testsupport/fakeidp/deep": 50,
	}
	for pkg, floor := range want {
		got, _ := floorFor(rules, defaultFloor, pkg)
		if got != floor {
			t.Errorf("%s: floor %v, want %v", pkg, got, floor)
		}
	}
}

// Every rule states why; a floor of 0 is only for suites measured elsewhere.
func TestRepoFloorsZeroFloorsAreRare(t *testing.T) {
	zero := 0
	for _, rule := range repoFloorRules(t) {
		if rule.Reason == "" {
			t.Errorf("line %d has no reason", rule.Line)
		}
		if rule.Floor == 0 {
			zero++
		}
	}
	if zero > 3 {
		t.Errorf("%d rules exempt packages with floor 0; the gate is being hollowed out", zero)
	}
}

// testJob returns the test job's text from ci.yml.
func testJob(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(repoWorkflow)
	if err != nil {
		t.Fatalf("read the CI workflow: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	start := strings.Index(text, "\n  test:\n")
	if start < 0 {
		t.Fatal("ci.yml has no test job")
	}
	rest := text[start+1:]
	if next := regexp.MustCompile(`\n  [a-z][a-z0-9-]*:\n`).FindStringIndex(rest[1:]); next != nil {
		rest = rest[:next[0]+1]
	}
	return rest
}

// The test job gates on the floors after the skip budget, on the unit
// stream, even when the suite failed (the step reads what the suite wrote).
func TestWorkflowTestJobRunsCovfloor(t *testing.T) {
	job := testJob(t)
	budget := strings.Index(job, "go run ./cmd/skipbudget")
	gate := strings.Index(job, "go run ./cmd/covfloor < ../unit.json")
	if budget < 0 || gate < 0 {
		t.Fatalf("test job lacks the skip budget (%d) or the covfloor step (%d)", budget, gate)
	}
	if gate < budget {
		t.Error("the covfloor step runs before the skip budget")
	}
	step := job[strings.LastIndex(job[:gate], "- name:"):gate]
	for _, want := range []string{"working-directory: sidecar", "if: ${{ !cancelled() }}",
		"shell: bash"} {
		if !strings.Contains(step, want) {
			t.Errorf("covfloor step lacks %q:\n%s", want, step)
		}
	}
}

func TestLocalRunnerRunsCovfloorOnPG17(t *testing.T) {
	raw, err := os.ReadFile(repoRunner)
	if err != nil {
		t.Fatalf("read the local runner: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "go run ./cmd/covfloor") {
		t.Fatal("local-linux-suite.sh does not run covfloor")
	}
	if !strings.Contains(text, "covfloor=") {
		t.Error("local-linux-suite.sh does not record covfloor's status")
	}
}
