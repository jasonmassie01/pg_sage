package startup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ci-08: both Playwright suites run in CI. The mocked dashboard suite
// (sidecar/web/e2e) runs on every push in the test job; the root suite
// (e2e/) drives the real binary through a scripted full-surface fixture on
// nightly, tag and manual runs, and a release waits for it. Both fail on a
// skipped test.
//
// No concurrent-access or PostgreSQL surface: these tests read committed
// workflow and package files. The suites themselves run in CI (CHECK-01..03).

type pwStep struct {
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	If               string            `yaml:"if"`
	WorkingDirectory string            `yaml:"working-directory"`
	With             map[string]string `yaml:"with"`
	Env              map[string]string `yaml:"env"`
}

type pwJob struct {
	If    string   `yaml:"if"`
	Needs any      `yaml:"needs"`
	Steps []pwStep `yaml:"steps"`
}

func loadPlaywrightJobs(t *testing.T) map[string]pwJob {
	t.Helper()
	var wf struct {
		Jobs map[string]pwJob `yaml:"jobs"`
	}
	path := ciWorkflowPath(t)
	if err := yaml.Unmarshal([]byte(readRepoFile(t, path)), &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf.Jobs
}

// pwStepIndex returns the index of the first step whose run script contains
// every fragment, or -1.
func pwStepIndex(steps []pwStep, fragments ...string) int {
	return slices.IndexFunc(steps, func(s pwStep) bool {
		for _, f := range fragments {
			if !strings.Contains(s.Run, f) {
				return false
			}
		}
		return true
	})
}

func failureUpload(steps []pwStep, path string) bool {
	return slices.ContainsFunc(steps, func(s pwStep) bool {
		return strings.HasPrefix(s.Uses, "actions/upload-artifact@") &&
			strings.Contains(s.If, "failure()") && strings.Contains(s.With["path"], path)
	})
}

func TestPwStepIndexNeedsEveryFragment(t *testing.T) {
	steps := []pwStep{{Run: "npm ci"}, {Run: "npx playwright test --workers=1"}}
	if got := pwStepIndex(steps, "playwright test", "--workers=1"); got != 1 {
		t.Errorf("pwStepIndex = %d, want 1", got)
	}
	if got := pwStepIndex(steps, "playwright test", "--workers=2"); got != -1 {
		t.Errorf("pwStepIndex with an absent fragment = %d, want -1", got)
	}
	if got := pwStepIndex(nil, "npm ci"); got != -1 {
		t.Errorf("pwStepIndex on no steps = %d, want -1", got)
	}
}

func TestMockedPlaywrightSuiteRunsInTestJob(t *testing.T) {
	steps := loadPlaywrightJobs(t)["test"].Steps
	build := pwStepIndex(steps, "npm run build")
	run := pwStepIndex(steps, "npx playwright install --with-deps chromium",
		"npx playwright test --reporter=list,json", "playwright-skip-gate.mjs")
	if build < 0 || run < 0 {
		t.Fatalf("test job: frontend build step %d, Playwright step %d; want both", build, run)
	}
	if run < build {
		t.Errorf("Playwright step %d runs before the frontend build %d", run, build)
	}
	st := steps[run]
	if st.WorkingDirectory != "sidecar/web" || st.Env["CI"] == "" {
		t.Errorf("Playwright step dir %q, CI %q; want sidecar/web and CI set",
			st.WorkingDirectory, st.Env["CI"])
	}
	if !strings.HasPrefix(st.Env["PLAYWRIGHT_JSON_OUTPUT_NAME"], "test-results/") {
		t.Errorf("JSON report %q, want it under test-results/ (uploaded on failure)",
			st.Env["PLAYWRIGHT_JSON_OUTPUT_NAME"])
	}
	cached := slices.ContainsFunc(steps[:run], func(s pwStep) bool {
		return strings.HasPrefix(s.Uses, "actions/cache@") &&
			s.With["path"] == "~/.cache/ms-playwright" &&
			strings.Contains(s.With["key"], "hashFiles('sidecar/web/package-lock.json')")
	})
	if !cached {
		t.Error("no browser cache on ~/.cache/ms-playwright keyed on the web lockfile " +
			"before the Playwright step")
	}
	if !failureUpload(steps, "sidecar/web/test-results") {
		t.Error("test job does not upload sidecar/web/test-results on failure")
	}
	if pwStepIndex(steps, "node --test scripts/ci/") < 0 {
		t.Error("test job does not run the gate scripts' own tests (node --test scripts/ci/)")
	}
}

func TestFullSurfaceSuiteRunsOnNightlyTagAndManualRuns(t *testing.T) {
	jobs := loadPlaywrightJobs(t)
	job, ok := jobs["web-e2e-full"]
	if !ok {
		t.Fatal("ci.yml has no web-e2e-full job")
	}
	for _, want := range []string{"'schedule'", "'workflow_dispatch'",
		"startsWith(github.ref, 'refs/tags/v')"} {
		if !strings.Contains(job.If, want) {
			t.Errorf("web-e2e-full if %q does not name %s", job.If, want)
		}
	}
	if strings.Contains(job.If, "pull_request") {
		t.Errorf("web-e2e-full if %q runs on pull requests; it is nightly/tag/manual", job.If)
	}
	fixture := pwStepIndex(job.Steps, "scripts/ci/full-surface-fixture.sh 17")
	suite := pwStepIndex(job.Steps, "playwright test", "--workers=1",
		"--grep-invert 'encryption-key requirement'", "playwright-skip-gate.mjs")
	keyless := pwStepIndex(job.Steps, "playwright test", "--grep 'encryption-key requirement'",
		"playwright-skip-gate.mjs")
	inputs := pwStepIndex(job.Steps, "scripts/ci/full-surface-fixture-test.sh",
		"PG_SAGE_E2E_PG_CONTAINER")
	if fixture < 0 || suite < 0 || keyless < 0 || inputs < 0 {
		t.Fatalf("web-e2e-full steps: fixture %d, suite %d, keyless %d, input checks %d; "+
			"want all", fixture, suite, keyless, inputs)
	}
	if !(fixture < inputs && fixture < suite && suite < keyless) {
		t.Errorf("step order fixture %d, input checks %d, suite %d, keyless %d; want the "+
			"fixture first and the keyless server after the suite", fixture, inputs, suite, keyless)
	}
	if !failureUpload(job.Steps, "e2e/test-results") {
		t.Error("web-e2e-full does not upload e2e/test-results on failure")
	}
	if !slices.Contains(ciJob{Needs: jobs["release"].Needs}.needs(), "web-e2e-full") {
		t.Error("release does not wait for web-e2e-full")
	}
}

// e2eSourceFiles returns the committed root-suite files, without the
// installed packages and the run output.
func e2eSourceFiles(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(wave5SidecarRoot(t), "..", "e2e")
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "test-results") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() != "package-lock.json" {
			out = append(out, path)
		}
		return nil
	})
	if err != nil || len(out) == 0 {
		t.Fatalf("list %s: %v (found %d)", dir, err, len(out))
	}
	return out
}

// CHECK-04: the root suite names no hand-built audit container and no report
// outside the repository.
func TestRootE2ESuiteNeedsNoHandBuiltFixture(t *testing.T) {
	for _, path := range e2eSourceFiles(t) {
		body := readRepoFile(t, path)
		for _, banned := range []string{"pgsage_audit_20260904", "../tasks", "audit_browser"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s still names %q", filepath.Base(path), banned)
			}
		}
	}
	targets := readRepoFile(t, filepath.Join(wave5SidecarRoot(t), "..", "e2e",
		"fixture-targets.ts"))
	for _, want := range []string{"process.env.PG_SAGE_E2E_PG_CONTAINER", "'pgsage_e2e'"} {
		if !strings.Contains(targets, want) {
			t.Errorf("fixture-targets.ts does not contain %s", want)
		}
	}
}

// playwrightVersions returns the declared @playwright/test range and the
// version the lockfile resolves for one package directory.
func playwrightVersions(t *testing.T, dir string) (declared, locked string) {
	t.Helper()
	var pkg struct {
		DevDependencies map[string]string `json:"devDependencies"`
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	for file, into := range map[string]any{"package.json": &pkg, "package-lock.json": &lock} {
		body := readRepoFile(t, filepath.Join(dir, file))
		if err := json.Unmarshal([]byte(body), into); err != nil {
			t.Fatalf("parse %s/%s: %v", dir, file, err)
		}
	}
	return pkg.DevDependencies["@playwright/test"],
		lock.Packages["node_modules/@playwright/test"].Version
}

func TestBothSuitesPinTheSamePlaywright(t *testing.T) {
	root := filepath.Join(wave5SidecarRoot(t), "..")
	webDeclared, webLocked := playwrightVersions(t, filepath.Join(root, "sidecar", "web"))
	e2eDeclared, e2eLocked := playwrightVersions(t, filepath.Join(root, "e2e"))
	if webDeclared == "" || webLocked == "" {
		t.Fatalf("sidecar/web declares %q, locks %q; want both", webDeclared, webLocked)
	}
	if e2eDeclared != webDeclared || e2eLocked != webLocked {
		t.Errorf("e2e declares %q and locks %q; sidecar/web declares %q and locks %q",
			e2eDeclared, e2eLocked, webDeclared, webLocked)
	}
}
