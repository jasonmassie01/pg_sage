package startup

import (
	"slices"
	"strings"
	"testing"
)

// benches-05: the integration matrix (PG14, 15, 16, 18 and their
// AgentSafetyBench baseline comparison) gates releases. A failure on one
// major must stop the v* tag from shipping a release or a versioned image;
// a master push still publishes :edge under the image job's own rule.

// No concurrent-access tests: these parse a committed file.

func TestReleaseNeedsIntegrationMatrix(t *testing.T) {
	jobs := ciJobs(t)
	matrix, ok := jobs["integration-matrix"]
	if !ok {
		t.Fatal("ci.yml has no integration-matrix job")
	}
	// The matrix must run on tag pushes, or release would wait on a skip.
	if matrix.If != "" {
		t.Fatalf("integration-matrix if %q; it must run on every push, tags included",
			matrix.If)
	}
	release := jobs["release"]
	if !slices.Contains(release.needs(), "integration-matrix") {
		t.Fatalf("release needs %v, want integration-matrix", release.needs())
	}
	// Without always() a failed or skipped need skips the release.
	if strings.Contains(release.If, "always()") {
		t.Fatalf("release if %q contains always()", release.If)
	}
}

func TestDockerTagRequiresIntegrationMatrix(t *testing.T) {
	docker := ciJobs(t)["docker"]
	if !slices.Contains(docker.needs(), "integration-matrix") {
		t.Fatalf("docker needs %v, want integration-matrix", docker.needs())
	}
	cond := expr(docker.If)
	// A tag needs the matrix; master (:edge) does not wait on its result.
	const tagClause = "(github.ref == 'refs/heads/master' || " +
		"needs.integration-matrix.result == 'success')"
	if !strings.Contains(cond, tagClause) {
		t.Fatalf("docker if %q lacks %q", cond, tagClause)
	}
	if strings.Count(cond, "needs.integration-matrix.result") != 1 {
		t.Fatalf("docker if %q must name the matrix result once, in the tag clause", cond)
	}
}

// The bench steps say what fails them now: the baseline comparison, not
// only machinery errors.
func TestSafetyBenchCommentsDescribeTheBaselineGate(t *testing.T) {
	text := expr(ciWorkflowText(t))
	if strings.Contains(text, "Only bench machinery # errors fail") ||
		strings.Contains(text, "Only bench machinery errors fail") {
		t.Fatal("ci.yml still says only machinery errors fail AgentSafetyBench")
	}
	if strings.Count(text, "safety-bench/testdata/baseline.json") < 2 {
		t.Fatal("both AgentSafetyBench steps must name the baseline they gate on")
	}
}
