package docscontract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// docsWorkflow is the part of .github/workflows/docs.yml these checks read.
type docsWorkflow struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
			Tags     []string `yaml:"tags"`
		} `yaml:"push"`
		PullRequest *struct {
			Paths []string `yaml:"paths"`
		} `yaml:"pull_request"`
	} `yaml:"on"`
	// Triggers holds every key under on:, since a bare
	// `workflow_dispatch:` decodes to null and no struct field shows it.
	Triggers map[string]any         `yaml:"-"`
	Jobs     map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	If    string `yaml:"if"`
	Steps []struct {
		Name string `yaml:"name"`
		If   string `yaml:"if"`
		Run  string `yaml:"run"`
	} `yaml:"steps"`
}

// The job guards: the strict build runs on pull requests, the deploy on
// everything else (release tags and manual dispatch).
const (
	prOnlyGuard = "github.event_name == 'pull_request'"
	notPRGuard  = "github.event_name != 'pull_request'"
)

var exactPin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*==[0-9][0-9A-Za-z.+-]*$`)

func loadDocsWorkflow(t *testing.T) docsWorkflow {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".github", "workflows", "docs.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf docsWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var raw struct {
		On map[string]any `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse %s triggers: %v", path, err)
	}
	wf.Triggers = raw.On
	if len(wf.Jobs) == 0 || len(wf.Triggers) == 0 {
		t.Fatalf("%s has no jobs or no triggers", path)
	}
	return wf
}

func TestDocsWorkflowBuildsStrictOnPullRequests(t *testing.T) {
	wf := loadDocsWorkflow(t)
	if wf.On.PullRequest == nil {
		t.Fatal("docs workflow has no pull_request trigger; broken docs merge unseen")
	}
	paths := strings.Join(wf.On.PullRequest.Paths, " ")
	for _, want := range []string{"docs/**", "mkdocs.yml", "requirements-docs.txt"} {
		if !strings.Contains(paths, want) {
			t.Errorf("pull_request paths %v omit %s", wf.On.PullRequest.Paths, want)
		}
	}
	if !workflowRuns(wf, "mkdocs build --strict") {
		t.Error("no docs workflow step runs `mkdocs build --strict`")
	}
	built := false
	for _, job := range wf.Jobs {
		if job.If == prOnlyGuard && jobRuns(job, "mkdocs build --strict") {
			built = true
		}
	}
	if !built {
		t.Errorf("no job guarded by %q runs `mkdocs build --strict`; PRs are not checked",
			prOnlyGuard)
	}
}

func TestDocsWorkflowDeploysOnlyReleases(t *testing.T) {
	wf := loadDocsWorkflow(t)
	if len(wf.On.Push.Branches) > 0 {
		t.Errorf("docs deploy on branch push %v; the site must match the latest release",
			wf.On.Push.Branches)
	}
	if strings.Join(wf.On.Push.Tags, ",") != "v*,!v*-*" {
		t.Errorf("push tags = %v, want [v* !v*-*]: release tags, no pre-releases",
			wf.On.Push.Tags)
	}
	if _, ok := wf.Triggers["workflow_dispatch"]; !ok {
		t.Error("docs workflow lost workflow_dispatch")
	}
	deploys := 0
	for name, job := range wf.Jobs {
		for _, step := range job.Steps {
			if !strings.Contains(step.Run, "gh-deploy") {
				continue
			}
			deploys++
			if job.If != notPRGuard {
				t.Errorf("job %s deploys under if %q, want %q", name, job.If, notPRGuard)
			}
			if step.If != "" {
				t.Errorf("job %s gates gh-deploy on step if %q; guard the job", name, step.If)
			}
		}
	}
	if deploys != 1 {
		t.Errorf("docs workflow has %d gh-deploy steps, want 1", deploys)
	}
}

func TestDocsWorkflowInstallsPinnedRequirements(t *testing.T) {
	wf := loadDocsWorkflow(t)
	if !workflowRuns(wf, "pip install -r requirements-docs.txt") {
		t.Error("docs workflow does not install from requirements-docs.txt")
	}
	if workflowRuns(wf, "pip install mkdocs") {
		t.Error("docs workflow still installs an unpinned mkdocs package")
	}
}

func TestDocsRequirementsArePinned(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "requirements-docs.txt"))
	if err != nil {
		t.Fatalf("read requirements-docs.txt: %v", err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !exactPin.MatchString(line) {
			t.Errorf("requirement %q is not an exact name==version pin", line)
			continue
		}
		name, _, _ := strings.Cut(line, "==")
		names[strings.ToLower(name)] = true
	}
	for _, want := range []string{"mkdocs", "mkdocs-material", "pymdown-extensions"} {
		if !names[want] {
			t.Errorf("requirements-docs.txt does not pin %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "requirements-docs.txt")); err == nil {
		t.Error("docs/requirements-docs.txt exists; MkDocs would publish it")
	}
}

func TestExactPinPattern(t *testing.T) {
	for _, ok := range []string{"mkdocs==1.6.1", "mkdocs-material==9.6.23", "Markdown==3.8.2"} {
		if !exactPin.MatchString(ok) {
			t.Errorf("exactPin rejected %q", ok)
		}
	}
	for _, bad := range []string{"mkdocs", "mkdocs>=1.6", "mkdocs~=1.6", "mkdocs==", "==1.0",
		"mkdocs==1.6.*", "mkdocs == 1.6.1"} {
		if exactPin.MatchString(bad) {
			t.Errorf("exactPin accepted %q", bad)
		}
	}
}

func workflowRuns(wf docsWorkflow, command string) bool {
	for _, job := range wf.Jobs {
		if jobRuns(job, command) {
			return true
		}
	}
	return false
}

func jobRuns(job workflowJob, command string) bool {
	for _, step := range job.Steps {
		if strings.Contains(step.Run, command) {
			return true
		}
	}
	return false
}
