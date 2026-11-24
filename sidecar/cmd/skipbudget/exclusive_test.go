package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ci-12: a test that needs the exclusive server and does not get it skips
// with "exclusive-cluster:", and no allow-list rule may accept that skip,
// so a CI job that loses the server fails its skip budget.

// exclusiveSkipStream is a recorded `go test -json` excerpt of
// testdb.Exclusive skipping on a run without SAGE_TEST_EXCLUSIVE_URL.
const exclusiveSkipStream = `{"Action":"run","Package":"github.com/pg-sage/sidecar/` +
	`internal/testdb","Test":"TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer"}
{"Action":"output","Package":"github.com/pg-sage/sidecar/internal/testdb",` +
	`"Test":"TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer",` +
	`"Output":"=== RUN   TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer\n"}
{"Action":"output","Package":"github.com/pg-sage/sidecar/internal/testdb",` +
	`"Test":"TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer",` +
	`"Output":"    exclusive_db_test.go:70: exclusive-cluster: happy path needs ` +
	`SAGE_TEST_EXCLUSIVE_URL\n"}
{"Action":"output","Package":"github.com/pg-sage/sidecar/internal/testdb",` +
	`"Test":"TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer",` +
	`"Output":"--- SKIP: TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer (0.00s)\n"}
{"Action":"skip","Package":"github.com/pg-sage/sidecar/internal/testdb",` +
	`"Test":"TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer","Elapsed":0}
`

func TestRepoAllowlistRejectsTheExclusiveClusterSkip(t *testing.T) {
	for _, pg := range []string{"14", "17"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-allow", repoAllowlist, "-pg", pg, "-stale", "off"},
			strings.NewReader(exclusiveSkipStream), &stdout, &stderr)
		if code != 1 {
			t.Fatalf("pg %s: exit %d, want 1 (stdout=%q stderr=%q)",
				pg, code, stdout.String(), stderr.String())
		}
		for _, want := range []string{
			"UNLISTED internal/testdb TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer:",
			"exclusive-cluster: happy path needs SAGE_TEST_EXCLUSIVE_URL",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("pg %s: output %q lacks %q", pg, stdout.String(), want)
			}
		}
	}
	for _, rule := range repoRules(t) {
		if strings.Contains(rule.Text, "exclusive-cluster") ||
			strings.Contains(rule.Text, "EXCLUSIVE") {
			t.Errorf("line %d (%s) allows a skip for the exclusive server", rule.Line, rule.Text)
		}
	}
}

// workflowJobs splits ci.yml into its top-level jobs, keyed by job id.
func workflowJobs(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(repoWorkflow)
	if err != nil {
		t.Fatalf("read the CI workflow: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	_, jobsText, ok := strings.Cut(text, "\njobs:\n")
	if !ok {
		t.Fatal("ci.yml has no jobs: section")
	}
	header := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\n`)
	idx := header.FindAllStringSubmatchIndex(jobsText, -1)
	jobs := make(map[string]string, len(idx))
	for i, m := range idx {
		end := len(jobsText)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		jobs[jobsText[m[2]:m[3]]] = jobsText[m[0]:end]
	}
	return jobs
}

// Every job that starts the shared test server also starts the exclusive
// one from the same image, exports its URL and checks it like the first.
func TestWorkflowJobsStartTheExclusiveServer(t *testing.T) {
	jobs := workflowJobs(t)
	withService := 0
	for name, job := range jobs {
		if !strings.Contains(job, "\n      postgres:\n") {
			continue
		}
		withService++
		image := regexp.MustCompile(`\n      postgres:\n        image: ([^\n]+)`).
			FindStringSubmatch(job)
		exclusive := regexp.MustCompile(`\n      postgres-exclusive:\n        image: ([^\n]+)`).
			FindStringSubmatch(job)
		switch {
		case exclusive == nil:
			t.Errorf("job %s has no postgres-exclusive service", name)
			continue
		case image == nil || image[1] != exclusive[1]:
			t.Errorf("job %s: postgres-exclusive image %q differs from postgres %v",
				name, exclusive[1], image)
		}
		if !regexp.MustCompile(`\n      SAGE_TEST_EXCLUSIVE_URL: postgres://postgres:postgres@`+
			`127.0.0.1:5433/postgres\?sslmode=disable\n`).MatchString(job) ||
			!strings.Contains(job, "- 5433:5432") {
			t.Errorf("job %s does not export SAGE_TEST_EXCLUSIVE_URL on port 5433", name)
		}
		if !strings.Contains(job, "verify-test-postgres.sh "+
			"${{ job.services.postgres-exclusive.id }}") {
			t.Errorf("job %s does not run verify-test-postgres.sh on postgres-exclusive", name)
		}
	}
	// test, integration-matrix, perfgate, bench, safety-bench, shuffle, bench-live.
	if withService < 7 {
		t.Fatalf("found %d jobs with a postgres service, want at least 7", withService)
	}
}
