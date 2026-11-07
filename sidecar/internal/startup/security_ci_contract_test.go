package startup

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Vulnerability gate contract (ci-01, also security-04): a security job
// runs govulncheck, npm audit and (on master and tags) a Trivy scan of the
// image; nothing is published without it; the workflow token defaults to
// read-only; Dependabot watches every ecosystem that ships.
//
// No concurrency, state-transition or PostgreSQL surface: these tests read
// workflow files. The error-propagation case (a reachable vulnerability
// fails the job) is exercised in CI on a throwaway branch.

type secStep struct {
	Name             string            `yaml:"name"`
	If               string            `yaml:"if"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	WorkingDirectory string            `yaml:"working-directory"`
	With             map[string]string `yaml:"with"`
	ContinueOnError  string            `yaml:"continue-on-error"`
}

type secJob struct {
	If              string            `yaml:"if"`
	Permissions     map[string]string `yaml:"permissions"`
	Steps           []secStep         `yaml:"steps"`
	ContinueOnError string            `yaml:"continue-on-error"`
}

type secWorkflow struct {
	Permissions map[string]string    `yaml:"permissions"`
	Jobs        map[string]secJob    `yaml:"jobs"`
	RawJobs     map[string]yaml.Node `yaml:"-"`
}

var (
	exactVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	// softFail matches shell that turns a failing scanner into a pass.
	softFail = regexp.MustCompile(`\|\|\s*(true|:|exit\s+0)(\s|;|\)|$)|set\s+\+e`)
)

func loadSecWorkflow(t *testing.T) secWorkflow {
	t.Helper()
	path := ciWorkflowPath(t)
	raw := []byte(readRepoFile(t, path))
	var wf secWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var nodes struct {
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &nodes); err != nil {
		t.Fatalf("parse %s jobs: %v", path, err)
	}
	wf.RawJobs = nodes.Jobs
	return wf
}

func securityJob(t *testing.T) secJob {
	t.Helper()
	job, ok := loadSecWorkflow(t).Jobs["security"]
	if !ok {
		t.Fatal("ci.yml has no security job")
	}
	return job
}

// stepRunning returns the steps whose run script contains needle.
func stepRunning(job secJob, needle string) []secStep {
	var out []secStep
	for _, s := range job.Steps {
		if strings.Contains(s.Run, needle) {
			out = append(out, s)
		}
	}
	return out
}

func TestCIWorkflowTokenDefaultsToReadOnly(t *testing.T) {
	got := loadSecWorkflow(t).Permissions
	if len(got) != 1 || got["contents"] != "read" {
		t.Fatalf("ci.yml top-level permissions = %v, want exactly contents: read", got)
	}
}

// With a read-only default, a job that pulls the repository's CI images
// from ghcr.io needs packages: read of its own.
func TestCIJobsPullingRegistryImagesCanReadPackages(t *testing.T) {
	wf := loadSecWorkflow(t)
	pulling := 0
	for name, node := range wf.RawJobs {
		text, err := yaml.Marshal(&node)
		if err != nil {
			t.Fatalf("render job %s: %v", name, err)
		}
		if !strings.Contains(string(text), "ghcr.io/jasonmassie01/pg_sage-ci-pgvector") {
			continue
		}
		pulling++
		if p := wf.Jobs[name].Permissions["packages"]; p != "read" && p != "write" {
			t.Errorf("job %s pulls ghcr.io CI images but has packages: %q", name, p)
		}
	}
	if pulling < 3 {
		t.Errorf("found %d jobs pulling CI images, want test, integration-matrix, ...", pulling)
	}
}

func TestSecurityJobRunsGovulncheck(t *testing.T) {
	job := securityJob(t)
	if job.If != "" {
		t.Errorf("security job is conditional (%q); it must run on every event", job.If)
	}
	steps := stepRunning(job, "govulncheck ./...")
	if len(steps) != 1 {
		t.Fatalf("security job has %d govulncheck ./... steps, want 1", len(steps))
	}
	if steps[0].WorkingDirectory != "sidecar" {
		t.Errorf("govulncheck runs in %q, want sidecar", steps[0].WorkingDirectory)
	}
	if steps[0].If != "" {
		t.Errorf("govulncheck step is conditional (%q)", steps[0].If)
	}
	install := stepRunning(job, "golang.org/x/vuln/cmd/govulncheck@")
	if len(install) != 1 {
		t.Fatalf("security job installs govulncheck %d times, want 1", len(install))
	}
	_, version, _ := strings.Cut(install[0].Run, "govulncheck@")
	version = strings.Fields(version)[0]
	if !exactVersion.MatchString(version) {
		t.Errorf("govulncheck version %q is not an exact vX.Y.Z", version)
	}
	if !slices.ContainsFunc(job.Steps, func(s secStep) bool {
		return strings.HasPrefix(s.Uses, "actions/setup-go@") &&
			s.With["go-version-file"] == "sidecar/go.mod"
	}) {
		t.Error("security job does not set up Go from sidecar/go.mod")
	}
}

func TestSecurityJobAuditsShippedNpmDependencies(t *testing.T) {
	steps := stepRunning(securityJob(t), "npm audit --omit=dev --audit-level=high")
	if len(steps) != 1 {
		t.Fatalf("security job has %d npm audit steps, want 1", len(steps))
	}
	if steps[0].WorkingDirectory != "sidecar/web" {
		t.Errorf("npm audit runs in %q, want sidecar/web", steps[0].WorkingDirectory)
	}
	if steps[0].If != "" {
		t.Errorf("npm audit step is conditional (%q)", steps[0].If)
	}
}

func TestSecurityJobScansImageOnMasterAndTags(t *testing.T) {
	job := securityJob(t)
	var trivy []secStep
	for _, s := range job.Steps {
		if strings.HasPrefix(s.Uses, "aquasecurity/trivy-action@") {
			trivy = append(trivy, s)
		}
	}
	if len(trivy) != 1 {
		t.Fatalf("security job has %d trivy steps, want 1", len(trivy))
	}
	scan := trivy[0]
	for key, want := range map[string]string{
		"exit-code": "1", "ignore-unfixed": "true", "severity": "HIGH,CRITICAL",
	} {
		if scan.With[key] != want {
			t.Errorf("trivy %s = %q, want %q", key, scan.With[key], want)
		}
	}
	if !exactVersion.MatchString(scan.With["version"]) {
		t.Errorf("trivy version = %q, want an exact vX.Y.Z", scan.With["version"])
	}
	cond := expr(scan.If)
	for _, want := range []string{
		"github.event_name == 'push'", "refs/heads/master", "startsWith(github.ref, 'refs/tags/v')",
	} {
		if !strings.Contains(cond, want) {
			t.Errorf("trivy step if %q is missing %q", cond, want)
		}
	}
	ref := scan.With["image-ref"]
	built := stepRunning(job, "docker build")
	if ref == "" || len(built) != 1 || !strings.Contains(built[0].Run, ref) {
		t.Errorf("trivy scans %q, which no single docker build step in the job builds", ref)
	}
	if len(built) == 1 && expr(built[0].If) != cond {
		t.Errorf("image build if %q differs from the scan's %q", built[0].If, scan.If)
	}
}

// The gate must stay a gate: continue-on-error on the job or a step, or a
// run script that swallows a scanner's exit code, lets docker and release
// see a "success" past a finding.
func TestSecurityJobCannotSoftFail(t *testing.T) {
	job := securityJob(t)
	if job.ContinueOnError != "" {
		t.Errorf("security job sets continue-on-error: %q", job.ContinueOnError)
	}
	for _, s := range job.Steps {
		label := s.Name
		if label == "" {
			label = s.Uses
		}
		if s.ContinueOnError != "" {
			t.Errorf("security step %q sets continue-on-error: %q", label, s.ContinueOnError)
		}
		if m := softFail.FindString(s.Run); m != "" {
			t.Errorf("security step %q swallows a failure with %q", label, m)
		}
	}
}

func TestSoftFailMatchesSwallowedExitCodes(t *testing.T) {
	cases := map[string]bool{
		"govulncheck ./... || true":               true,
		"govulncheck ./... ||true":                true,
		"npm audit --audit-level=high || :":       true,
		"govulncheck ./... || exit 0":             true,
		"set +e\ngovulncheck ./...":               true,
		"govulncheck ./...":                       false,
		"npm audit --omit=dev --audit-level=high": false,
		"test -f x || exit 1":                     false,
		"":                                        false,
	}
	for script, want := range cases {
		if got := softFail.MatchString(script); got != want {
			t.Errorf("softFail.MatchString(%q) = %v, want %v", script, got, want)
		}
	}
}

func TestReleaseAndDockerNeedSecurity(t *testing.T) {
	jobs := ciJobs(t)
	for _, name := range []string{"release", "docker"} {
		job, ok := jobs[name]
		if !ok {
			t.Fatalf("ci.yml has no %s job", name)
		}
		if !slices.Contains(job.needs(), "security") {
			t.Errorf("%s.needs = %v, missing security", name, job.needs())
		}
		cond := expr(job.If)
		overrides := strings.Contains(cond, "always()") || strings.Contains(cond, "!cancelled()")
		if overrides && !strings.Contains(cond, "needs.security.result == 'success'") {
			t.Errorf("%s.if %q runs past a failed security job", name, cond)
		}
	}
}

type dependabotUpdate struct {
	Ecosystem   string   `yaml:"package-ecosystem"`
	Directory   string   `yaml:"directory"`
	Directories []string `yaml:"directories"`
	Schedule    struct {
		Interval string `yaml:"interval"`
	} `yaml:"schedule"`
}

func TestDependabotWatchesEveryShippedEcosystem(t *testing.T) {
	path := filepath.Join(wave5SidecarRoot(t), "..", ".github", "dependabot.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg struct {
		Version int                `yaml:"version"`
		Updates []dependabotUpdate `yaml:"updates"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if cfg.Version != 2 {
		t.Errorf("%s version = %d, want 2", path, cfg.Version)
	}
	got := map[string][]string{}
	for _, u := range cfg.Updates {
		if u.Schedule.Interval != "weekly" {
			t.Errorf("%s %s interval = %q, want weekly", path, u.Ecosystem, u.Schedule.Interval)
		}
		dirs := append(slices.Clone(u.Directories), u.Directory)
		for _, d := range dirs {
			if d != "" {
				got[u.Ecosystem] = append(got[u.Ecosystem], d)
			}
		}
	}
	want := map[string][]string{
		"gomod":          {"/sidecar"},
		"npm":            {"/sidecar/web"},
		"github-actions": {"/"},
		"docker":         {"/deploy/databricks-apps", "/sidecar"},
	}
	if len(got) != len(want) {
		t.Errorf("%s covers %d ecosystems %v, want %d", path, len(got), got, len(want))
	}
	for eco, dirs := range want {
		have := slices.Sorted(slices.Values(got[eco]))
		if !slices.Equal(have, dirs) {
			t.Errorf("%s %s directories = %v, want %v", path, eco, have, dirs)
		}
	}
}
