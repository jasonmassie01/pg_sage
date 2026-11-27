package startup

import (
	"sort"
	"strings"
	"testing"
)

// ci-05 review: a job's own permissions block replaces the top-level
// contents: read, so a job that checks out the repository and names its
// own scopes must name contents: read too. Without it the checkout works
// only while the repository is public.
//
// No PostgreSQL or concurrent-access surface: this reads ci.yml.

// checkoutProblems names the jobs that check out the repository from a
// permissions block that does not grant contents read or write.
func checkoutProblems(jobs map[string]relJob) []string {
	var out []string
	for name, job := range jobs {
		if job.Permissions == nil || stepIndex(job, "actions/checkout@") < 0 {
			continue
		}
		if level := job.Permissions["contents"]; level != "read" && level != "write" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func TestJobsThatCheckOutCanReadContents(t *testing.T) {
	wf := loadRelWorkflow(t)
	if got := checkoutProblems(wf.Jobs); len(got) > 0 {
		t.Fatalf("jobs check out the repository without contents: read: %s",
			strings.Join(got, ", "))
	}
	docker := relJobNamed(t, wf, "docker")
	if stepIndex(docker, "actions/checkout@") < 0 || docker.Permissions == nil {
		t.Fatal("docker no longer checks out with its own permissions block; " +
			"the contract above would check nothing for it")
	}
}

func TestCheckoutProblemsFlagsMissingContents(t *testing.T) {
	checkout := []relStep{{Uses: "actions/checkout@v4"}}
	jobs := map[string]relJob{
		"inherits":   {Steps: checkout},
		"reads":      {Permissions: map[string]string{"contents": "read"}, Steps: checkout},
		"writes":     {Permissions: map[string]string{"contents": "write"}, Steps: checkout},
		"no-content": {Permissions: map[string]string{"packages": "write"}, Steps: checkout},
		"none":       {Permissions: map[string]string{"contents": "none"}, Steps: checkout},
		"no-checkout": {Permissions: map[string]string{"packages": "write"},
			Steps: []relStep{{Run: "echo hi"}}},
	}
	got := strings.Join(checkoutProblems(jobs), ",")
	if got != "no-content,none" {
		t.Fatalf("checkoutProblems = %q, want %q", got, "no-content,none")
	}
	if got := checkoutProblems(nil); len(got) != 0 {
		t.Fatalf("checkoutProblems(nil) = %v, want none", got)
	}
}
