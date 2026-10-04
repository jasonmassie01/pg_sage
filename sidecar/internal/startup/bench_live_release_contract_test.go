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

// Roadmap 2.4 release contract (owner decision 2026-10-04): the
// live-model arm also runs on every v* tag push, signed keyless the same
// way as the nightly run, and its report is attached to the GitHub
// release (pgincidentbench-live.json, .json.sigstore.json and .md) when
// it finishes. It never blocks the release: neither release nor docker
// needs it, and only the attaching job waits for both. The workflow
// names no model: the default lives in code next to its prices
// (sre-bench), and a repository variable overrides it.

type liveCIStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type liveCIJob struct {
	Needs       any               `yaml:"needs"`
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Env         map[string]string `yaml:"env"`
	Steps       []liveCIStep      `yaml:"steps"`
}

func (j liveCIJob) needs() []string { return ciJob{Needs: j.Needs}.needs() }

func ciWorkflowText(t *testing.T) string {
	t.Helper()
	path := filepath.Join(wave5SidecarRoot(t), "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func liveCIJobs(t *testing.T) map[string]liveCIJob {
	t.Helper()
	var wf struct {
		Jobs map[string]liveCIJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(ciWorkflowText(t)), &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	if _, ok := wf.Jobs["bench-live"]; !ok {
		t.Fatal("ci.yml has no bench-live job")
	}
	return wf.Jobs
}

const tagPush = "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')"

func TestBenchLiveRunsOnReleaseTagsToo(t *testing.T) {
	cond := expr(liveCIJobs(t)["bench-live"].If)
	for _, want := range []string{"github.event_name == 'schedule'",
		"github.event_name == 'workflow_dispatch'", tagPush} {
		if !strings.Contains(cond, want) {
			t.Errorf("bench-live if = %q, want it to include %q", cond, want)
		}
	}
	if strings.Contains(cond, "pull_request") || strings.Contains(cond, "refs/heads/") {
		t.Fatalf("bench-live must not run on a pull request or a branch push: %q", cond)
	}
}

// Nothing the release or the image waits for depends on the live arm.
func TestReleaseAndImageNeverWaitForTheLiveArm(t *testing.T) {
	jobs := liveCIJobs(t)
	var waits func(name string, seen map[string]bool) bool
	waits = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		for _, n := range jobs[name].needs() {
			if n == "bench-live" || waits(n, seen) {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"release", "docker"} {
		job, ok := jobs[name]
		if !ok {
			t.Fatalf("ci.yml has no %s job", name)
		}
		if waits(name, map[string]bool{}) || strings.Contains(job.If, "bench-live") {
			t.Errorf("%s waits for bench-live (needs %v, if %q)", name, job.needs(), job.If)
		}
	}
	if p := jobs["bench-live"].Permissions; p["contents"] != "read" || p["id-token"] != "write" {
		t.Fatalf("bench-live permissions = %v (release writes live elsewhere)", p)
	}
}

func liveSignStep(t *testing.T, job liveCIJob) liveCIStep {
	t.Helper()
	for _, st := range job.Steps {
		if strings.Contains(st.Run, "cosign sign-blob") {
			return st
		}
	}
	t.Fatal("bench-live has no signing step")
	return liveCIStep{}
}

func TestLiveReportIsSignedOnTagsTheSameWay(t *testing.T) {
	job := liveCIJobs(t)["bench-live"]
	sign := liveSignStep(t, job)
	cond := expr(sign.If)
	if !strings.Contains(cond, "github.ref == 'refs/heads/master'") ||
		!strings.Contains(cond, "startsWith(github.ref, 'refs/tags/v')") ||
		!strings.Contains(cond, "steps.secret.outputs.ready == 'true'") {
		t.Fatalf("signing if = %q, want master or a v* tag behind the secret check", cond)
	}
	for _, want := range []string{"cosign verify-blob", "--certificate-identity",
		"bench verify --commit"} {
		if !strings.Contains(sign.Run, want) {
			t.Errorf("signing step lacks %q", want)
		}
	}
	if sign.ID == "" || !strings.Contains(sign.Run, `echo "signed=true" >> "$GITHUB_OUTPUT"`) ||
		job.Outputs["signed"] != "${{ steps."+sign.ID+".outputs.signed }}" {
		t.Fatalf("bench-live must output whether it signed: outputs %v, step id %q",
			job.Outputs, sign.ID)
	}
	// The tag run's report names the version it scored, like the
	// release bench, so the release's sidecar can match it.
	if v := job.Env["SAGE_BENCH_PG_SAGE_VERSION"]; !strings.Contains(v,
		"startsWith(github.ref, 'refs/tags/v')") || !strings.Contains(v, "github.ref_name") {
		t.Fatalf("SAGE_BENCH_PG_SAGE_VERSION = %q", v)
	}
}

var liveAssets = []string{"pgincidentbench-live.json",
	"pgincidentbench-live.json.sigstore.json", "pgincidentbench-live.md"}

func TestSignedLiveReportIsAttachedToTheRelease(t *testing.T) {
	jobs := liveCIJobs(t)
	var name string
	for n, j := range jobs {
		if slices.Contains(j.needs(), "bench-live") {
			if name != "" {
				t.Fatalf("both %s and %s need bench-live", name, n)
			}
			name = n
		}
	}
	job := jobs[name]
	if name == "" || !slices.Contains(job.needs(), "release") {
		t.Fatalf("no job attaches the live report after the release (%q needs %v)", name,
			job.needs())
	}
	cond := expr(job.If)
	if !strings.Contains(cond, "startsWith(github.ref, 'refs/tags/v')") ||
		!strings.Contains(cond, "needs.bench-live.outputs.signed == 'true'") ||
		strings.Contains(cond, "always()") {
		t.Fatalf("%s if = %q: only a signed tag run, after a successful release", name, cond)
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "write" {
		t.Fatalf("%s permissions = %v, want contents: write only", name, job.Permissions)
	}
	var download, upload *liveCIStep
	for i, st := range job.Steps {
		if strings.HasPrefix(st.Uses, "actions/download-artifact") &&
			st.With["name"] == "pgincidentbench-live" {
			download = &job.Steps[i]
		}
		if strings.Contains(st.Run, "gh release upload") {
			upload = &job.Steps[i]
		}
	}
	if download == nil || upload == nil {
		t.Fatalf("%s must download pgincidentbench-live and upload it", name)
	}
	for _, a := range liveAssets {
		if !strings.Contains(upload.Run, a) {
			t.Errorf("the upload does not attach %s", a)
		}
	}
	if !strings.Contains(upload.Run, "--clobber") ||
		!strings.Contains(upload.Run, `"$GITHUB_REF_NAME"`) ||
		upload.Env["GH_TOKEN"] != "${{ secrets.GITHUB_TOKEN }}" {
		t.Fatalf("upload step = %+v", upload)
	}
	for _, st := range job.Steps {
		for k, v := range st.Env {
			if strings.Contains(v, "PG_SAGE_BENCH_OPENAI_API_KEY") {
				t.Errorf("%s reads the live model key into %s", name, k)
			}
		}
	}
}

// Owner addition B: no model is hard-coded in the workflow. The repository
// variable overrides the default in code; its prices are variables too.
func TestLiveWorkflowNamesNoModel(t *testing.T) {
	text := ciWorkflowText(t)
	if m := regexp.MustCompile(`(?i)\bgpt-[a-z0-9.]+`).FindString(text); m != "" {
		t.Fatalf("ci.yml hard-codes the model %q", m)
	}
	var live liveCIStep
	for _, st := range liveCIJobs(t)["bench-live"].Steps {
		if strings.Contains(st.Run, "TestLiveModelArm") {
			live = st
		}
	}
	if got := live.Env["PG_SAGE_BENCH_LLM_MODEL"]; got !=
		"${{ vars.PG_SAGE_BENCH_OPENAI_MODEL }}" {
		t.Fatalf("PG_SAGE_BENCH_LLM_MODEL = %q, want the repository variable alone", got)
	}
	for _, k := range []string{"PG_SAGE_BENCH_LLM_USD_PER_MTOK_IN",
		"PG_SAGE_BENCH_LLM_USD_PER_MTOK_OUT"} {
		if got := live.Env[k]; got != "${{ vars."+k+" }}" {
			t.Errorf("%s = %q, want the repository variable alone (the default model's "+
				"prices live in code with it)", k, got)
		}
	}
}
