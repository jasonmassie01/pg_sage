package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The sample GitHub Action, its example workflow (CHECK-SRF-05) and
// docs/change-review.md (CHECK-SRF-08) are checked structurally here;
// actionlint and mkdocs build run on the same files outside go test.

const crRepoRoot = "../../.."

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(crRepoRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

type ghStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type ghWorkflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		If    string   `yaml:"if"`
		Steps []ghStep `yaml:"steps"`
	} `yaml:"jobs"`
}

var pinnedAction = regexp.MustCompile(
	`^jasonmassie01/pg_sage/integrations/github/pg-sage-review@(v\d+\.\d+\.\d+)$`)

func TestExampleWorkflowPinsAReleaseAndAsksForLeastPrivilege(t *testing.T) {
	var wf ghWorkflow
	if err := yaml.Unmarshal([]byte(readRepoFile(t,
		"integrations/github/example-workflow.yml")), &wf); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"contents": "read", "pull-requests": "write",
		"statuses": "write"}
	if len(wf.Permissions) != len(want) {
		t.Fatalf("permissions %v, want exactly %v", wf.Permissions, want)
	}
	for k, v := range want {
		if wf.Permissions[k] != v {
			t.Fatalf("permissions %v, want exactly %v", wf.Permissions, want)
		}
	}
	for _, trigger := range []string{"pull_request", "workflow_dispatch", "schedule"} {
		if _, ok := wf.On[trigger]; !ok {
			t.Fatalf("triggers %v lack %s", wf.On, trigger)
		}
	}
	sched, _ := wf.On["schedule"].([]any)
	if len(sched) != 1 || sched[0].(map[string]any)["cron"] != "*/15 * * * *" {
		t.Fatalf("schedule %v, want every 15 minutes", wf.On["schedule"])
	}
	var forkSkip, statusRun bool
	pins := 0
	for name, job := range wf.Jobs {
		for _, s := range job.Steps {
			if m := pinnedAction.FindStringSubmatch(s.Uses); m != nil {
				pins++
				if s.With["version"] != m[1] {
					t.Fatalf("job %s: action @%s installs %q", name, m[1], s.With["version"])
				}
			} else if strings.Contains(s.Uses, "pg-sage-review") {
				t.Fatalf("job %s uses an unpinned action %q", name, s.Uses)
			}
			forkSkip = forkSkip || strings.Contains(strings.ToLower(s.Name), "fork")
			if strings.Contains(job.If, "schedule") &&
				strings.Contains(s.Run, "pg_sage review status") {
				statusRun = true
			}
		}
	}
	if pins < 2 || !forkSkip || !statusRun {
		t.Fatalf("pins %d forkSkip %v statusRun %v: want the pinned action in both jobs, a "+
			"fork-PR skip step and a schedule job running 'pg_sage review status'", pins,
			forkSkip, statusRun)
	}
}

func TestActionInstallsAVerifiedReleaseAndFencesItsInputs(t *testing.T) {
	text := readRepoFile(t, "integrations/github/pg-sage-review/action.yml")
	var action struct {
		Inputs map[string]struct {
			Required bool   `yaml:"required"`
			Default  string `yaml:"default"`
		} `yaml:"inputs"`
		Runs struct {
			Using string   `yaml:"using"`
			Steps []ghStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal([]byte(text), &action); err != nil {
		t.Fatal(err)
	}
	if action.Runs.Using != "composite" {
		t.Fatalf("runs.using %q, want composite", action.Runs.Using)
	}
	for _, in := range []string{"version", "url", "database", "token"} {
		if !action.Inputs[in].Required {
			t.Errorf("input %s must be required", in)
		}
	}
	if action.Inputs["command"].Default != "submit" ||
		action.Inputs["fail-on"].Default != "risky" {
		t.Errorf("defaults: command %q fail-on %q", action.Inputs["command"].Default,
			action.Inputs["fail-on"].Default)
	}
	all := ""
	for _, s := range action.Runs.Steps {
		if strings.Contains(s.Run, "${{") {
			t.Errorf("step %q interpolates an expression into its script; pass it through "+
				"env instead", s.Name)
		}
		all += s.Run
	}
	for _, want := range []string{"releases/download", "checksums.txt", "sha256sum -c",
		"PG_SAGE_TOKEN_FILE", "umask 077", "pg_sage review submit --github",
		"pg_sage review status --github", "--changed-only", "::notice::"} {
		if !strings.Contains(all, want) {
			t.Errorf("the action's scripts lack %q", want)
		}
	}
	if strings.Contains(text, "secrets.") ||
		action.Inputs["github-token"].Default != "${{ github.token }}" {
		t.Errorf("the action must default to the runner's own token and read no secret "+
			"itself; github-token defaults to %q", action.Inputs["github-token"].Default)
	}
}

func TestChangeReviewDocsCoverTheSurfaces(t *testing.T) {
	doc := readRepoFile(t, "docs/change-review.md")
	if !strings.Contains(readRepoFile(t, "mkdocs.yml"), "change-review.md") {
		t.Fatal("mkdocs.yml nav must list change-review.md")
	}
	for _, want := range []string{
		// Agent tokens are minted for an agent principal since v2.4.0.
		"POST /api/v1/agents/{id}/tokens", `"sponsor_user_id"`,
		`"scopes":["read","propose"]`, `"databases":["orders"]`,
		"expires_in_days", "integrations/github/pg-sage-review", "pg_sage review submit",
		"pg_sage review status", "pg_sage review deployed", "PG_SAGE_TOKEN_FILE",
		"| 0 |", "| 1 |", "| 2 |", "| 3 |", "## What pg_sage reads",
		"## What pg_sage never does", "review_change", "report_change_deployed",
		"change_review_needs_dba", "/api/v1/databases/{db}/change-reviews",
		"pg_sage/db-review (", "GITHUB_TOKEN",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/change-review.md lacks %q", want)
		}
	}
	for _, sub := range []string{"submit", "status", "deployed"} {
		var o reviewOptions
		reviewFlagSet(sub, &o).VisitAll(func(f *flag.Flag) {
			if !strings.Contains(doc, "--"+f.Name) {
				t.Errorf("docs/change-review.md does not document %s --%s", sub, f.Name)
			}
		})
	}
}
