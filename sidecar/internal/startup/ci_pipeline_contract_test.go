package startup

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ci-03: the CI critical path. A second push to a pull request cancels
// the first run (master and tag runs never are, so bench-sign always
// signs a finished run); every Go and npm cache restores; each job runs
// the whole-module suite once; and bench-sign counts every bench shard.
//
// No concurrent-access or PostgreSQL surface: these tests read committed
// workflow files. Whether a cache actually hits is checked on a real run
// (CHECK-18, MANUAL).

type pipeStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type pipeJob struct {
	Steps []pipeStep `yaml:"steps"`
}

type pipeWorkflow struct {
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress string `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]pipeJob `yaml:"jobs"`
}

func parsePipeWorkflow(t *testing.T, name, body string) pipeWorkflow {
	t.Helper()
	var wf pipeWorkflow
	if err := yaml.Unmarshal([]byte(body), &wf); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return wf
}

func loadCIPipeline(t *testing.T) pipeWorkflow {
	t.Helper()
	path := ciWorkflowPath(t)
	return parsePipeWorkflow(t, path, readRepoFile(t, path))
}

// goTestCommands returns the fields of every go test command in a run
// script, with backslash continuations joined.
func goTestCommands(run string) [][]string {
	joined := strings.ReplaceAll(run, "\\\n", " ")
	var out [][]string
	for _, line := range strings.Split(joined, "\n") {
		for _, cmd := range strings.Split(line, "go test ")[1:] {
			fields := strings.Fields(cmd)
			if i := slices.IndexFunc(fields, func(f string) bool {
				return f == "||" || f == "&&" || f == ";" || f == "|"
			}); i >= 0 {
				fields = fields[:i]
			}
			out = append(out, fields)
		}
	}
	return out
}

// wholeModuleRuns returns a job's go test commands that test ./... .
func wholeModuleRuns(job pipeJob) [][]string {
	var out [][]string
	for _, st := range job.Steps {
		for _, cmd := range goTestCommands(st.Run) {
			if slices.Contains(cmd, "./...") {
				out = append(out, cmd)
			}
		}
	}
	return out
}

func TestGoTestCommandsParsesContinuations(t *testing.T) {
	run := "status=0\ngo test ./... -cover -tags=integration -count=1 \\\n" +
		"  -json > ../i.json || status=$?\necho go test-free line\n" +
		"go test ./e2e -tags=e2e"
	got := goTestCommands(run)
	if len(got) != 2 {
		t.Fatalf("goTestCommands = %q, want 2 commands", got)
	}
	want := []string{"./...", "-cover", "-tags=integration", "-count=1", "-json", ">",
		"../i.json"}
	if !slices.Equal(got[0], want) || !slices.Equal(got[1], []string{"./e2e", "-tags=e2e"}) {
		t.Fatalf("goTestCommands = %q", got)
	}
	if n := len(goTestCommands("")); n != 0 {
		t.Fatalf("empty script: %d commands, want 0", n)
	}
}

// Every untagged test runs again inside a -tags=integration run, so a job
// with two whole-module runs runs the untagged suite twice.
func TestCIRunsEachSuiteOnce(t *testing.T) {
	wf := loadCIPipeline(t)
	for name, job := range wf.Jobs {
		if runs := wholeModuleRuns(job); len(runs) > 1 {
			t.Errorf("job %s runs go test ./... %d times: %q", name, len(runs), runs)
		}
	}
	runs := wholeModuleRuns(wf.Jobs["test"])
	if len(runs) != 1 {
		t.Fatalf("test job: %d go test ./... runs, want 1", len(runs))
	}
	for _, flag := range []string{"-race", "-tags=integration", "-count=1", "-json"} {
		if !slices.Contains(runs[0], flag) {
			t.Errorf("test job go test ./... %q lacks %s", runs[0], flag)
		}
	}
}

var (
	jsonReport = regexp.MustCompile(`\.\./([a-z0-9-]+\.json)`)
	jsonWrite  = regexp.MustCompile(`>\s*\.\./([a-z0-9-]+\.json)`)
)

// The skip budget reads only reports an earlier step wrote: a cat of a
// missing file fails the step under pipefail and hides the real result.
func TestSkipBudgetReadsOnlyProducedReports(t *testing.T) {
	for name, job := range loadCIPipeline(t).Jobs {
		written := map[string]bool{}
		for _, st := range job.Steps {
			if st.Name == "Skip budget" {
				reads := jsonReport.FindAllStringSubmatch(st.Run, -1)
				if len(reads) == 0 {
					t.Errorf("%s / Skip budget reads no go test -json report", name)
				}
				for _, m := range reads {
					if !written[m[1]] {
						t.Errorf("%s / Skip budget reads ../%s, which no earlier step writes",
							name, m[1])
					}
				}
			}
			joined := strings.ReplaceAll(st.Run, "\\\n", " ")
			for _, m := range jsonWrite.FindAllStringSubmatch(joined, -1) {
				written[m[1]] = true
			}
		}
	}
}

var eventEquals = regexp.MustCompile(
	`^\$\{\{\s*github\.event_name\s*==\s*'([a-z_]+)'\s*\}\}$`)

// cancelsOn evaluates a cancel-in-progress value of the one accepted
// form, ${{ github.event_name == '<event>' }}, for an event.
func cancelsOn(value, event string) (bool, error) {
	m := eventEquals.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return false, fmt.Errorf("cancel-in-progress %q is not "+
			"${{ github.event_name == '<event>' }}", value)
	}
	return m[1] == event, nil
}

func TestCancelsOnRejectsOtherForms(t *testing.T) {
	for _, v := range []string{"", "true", "false", "${{ true }}",
		"${{ github.event_name != 'pull_request' }}",
		"${{ github.ref != 'refs/heads/master' }}"} {
		if _, err := cancelsOn(v, "pull_request"); err == nil {
			t.Errorf("cancelsOn(%q) accepted it", v)
		}
	}
	got, err := cancelsOn("${{ github.event_name == 'pull_request' }}", "pull_request")
	if err != nil || !got {
		t.Fatalf("cancelsOn(pull_request) = %v, %v; want true", got, err)
	}
}

// ghRun is the part of a run's github context a concurrency key reads.
type ghRun struct{ event, ref, runID string }

func (r ghRun) field(name string) (string, error) {
	switch name {
	case "event_name":
		return r.event, nil
	case "ref":
		return r.ref, nil
	case "run_id":
		return r.runID, nil
	}
	return "", fmt.Errorf("github.%s is not a concurrency key this contract evaluates", name)
}

var (
	plainKey = regexp.MustCompile(`^(.*)\$\{\{\s*github\.([a-z_]+)\s*\}\}(.*)$`)
	eventKey = regexp.MustCompile(`^(.*)\$\{\{\s*github\.event_name\s*==\s*'([a-z_]+)'` +
		`\s*&&\s*github\.([a-z_]+)\s*\|\|\s*github\.([a-z_]+)\s*\}\}(.*)$`)
)

// groupFor evaluates a concurrency group of one of two forms for a run:
// prefix${{ github.<key> }}suffix, or
// prefix${{ github.event_name == '<event>' && github.<a> || github.<b> }}suffix.
func groupFor(group string, run ghRun) (string, error) {
	if m := eventKey.FindStringSubmatch(group); m != nil {
		key := m[4]
		if run.event == m[2] {
			key = m[3]
		}
		v, err := run.field(key)
		return m[1] + v + m[5], err
	}
	if m := plainKey.FindStringSubmatch(group); m != nil {
		v, err := run.field(m[2])
		return m[1] + v + m[3], err
	}
	return "", fmt.Errorf("concurrency group %q has no github key this contract evaluates",
		group)
}

func TestGroupForEvaluatesBothForms(t *testing.T) {
	pr := ghRun{event: "pull_request", ref: "refs/pull/7/merge", runID: "11"}
	push := ghRun{event: "push", ref: "refs/heads/master", runID: "12"}
	ternary := "ci-${{ github.event_name == 'pull_request' && github.ref || github.run_id }}"
	for _, tc := range []struct {
		group string
		run   ghRun
		want  string
	}{
		{"ci-${{ github.ref }}", push, "ci-refs/heads/master"},
		{ternary, pr, "ci-refs/pull/7/merge"},
		{ternary, push, "ci-12"},
	} {
		got, err := groupFor(tc.group, tc.run)
		if err != nil || got != tc.want {
			t.Errorf("groupFor(%q, %s) = %q, %v; want %q", tc.group, tc.run.event, got,
				err, tc.want)
		}
	}
	for _, bad := range []string{"", "ci", "ci-${{ github.sha }}", "${{ inputs.x }}"} {
		if got, err := groupFor(bad, push); err == nil {
			t.Errorf("groupFor(%q) = %q, want an error", bad, got)
		}
	}
}

// State transitions: a newer push to the same pull request cancels the
// older run. A master push, a tag, the nightly and a manual run neither
// cancel nor queue behind another run (a shared group would queue them,
// and a third run would cancel the queued second one).
func TestCIConcurrencyCancelsOnlyPRs(t *testing.T) {
	c := loadCIPipeline(t).Concurrency
	key := func(r ghRun) string {
		g, err := groupFor(c.Group, r)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	pr1 := ghRun{event: "pull_request", ref: "refs/pull/7/merge", runID: "1"}
	pr2 := ghRun{event: "pull_request", ref: "refs/pull/7/merge", runID: "2"}
	other := ghRun{event: "pull_request", ref: "refs/pull/8/merge", runID: "3"}
	if key(pr1) != key(pr2) || key(pr1) == key(other) {
		t.Fatalf("group %q: PR 7 runs %q and %q, PR 8 %q; want one group per pull request",
			c.Group, key(pr1), key(pr2), key(other))
	}
	seen := map[string]string{}
	for i, event := range []string{"push", "push", "schedule", "workflow_dispatch", "push"} {
		ref := "refs/heads/master"
		if i == 4 {
			ref = "refs/tags/v2.6.0"
		}
		g := key(ghRun{event: event, ref: ref, runID: strconv.Itoa(100 + i)})
		if prev, dup := seen[g]; dup {
			t.Errorf("group %q: %s run %d shares group %q with %s", c.Group, event, i, g, prev)
		}
		seen[g] = event
		cancel, err := cancelsOn(c.CancelInProgress, event)
		if err != nil {
			t.Fatal(err)
		}
		if cancel {
			t.Errorf("event %s: cancel-in-progress is true, want false", event)
		}
	}
	if cancel, err := cancelsOn(c.CancelInProgress, "pull_request"); err != nil || !cancel {
		t.Errorf("pull_request: cancel-in-progress = %v, %v; want true", cancel, err)
	}
}

const (
	goSumPath       = "sidecar/go.sum"
	npmLockfilePath = "sidecar/web/package-lock.json"
)

// cacheProblems lists the setup-go steps whose module cache cannot
// restore (no root go.sum, so the path must be named) and the setup-node
// steps in jobs that run npm ci without an npm cache.
func cacheProblems(name, body string) ([]string, error) {
	var wf pipeWorkflow
	if err := yaml.Unmarshal([]byte(body), &wf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var out []string
	for jobName, job := range wf.Jobs {
		npmCI := slices.ContainsFunc(job.Steps, func(s pipeStep) bool {
			return strings.Contains(s.Run, "npm ci")
		})
		for _, st := range job.Steps {
			action, _, _ := strings.Cut(st.Uses, "@")
			switch {
			case action == "actions/setup-go" && (st.With["cache-dependency-path"] != goSumPath ||
				st.With["cache"] == "false"):
				out = append(out, fmt.Sprintf("%s / %s: setup-go cache %q, path %q; want %s",
					name, jobName, st.With["cache"], st.With["cache-dependency-path"], goSumPath))
			case action == "actions/setup-node" && npmCI && (st.With["cache"] != "npm" ||
				st.With["cache-dependency-path"] != npmLockfilePath):
				out = append(out, fmt.Sprintf("%s / %s: setup-node cache %q, path %q; "+
					"want npm, %s", name, jobName, st.With["cache"],
					st.With["cache-dependency-path"], npmLockfilePath))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func TestCacheProblemsFlagsInvalidSetups(t *testing.T) {
	body := `jobs:
  a:
    steps:
      - uses: actions/setup-go@v5
        with: {go-version-file: sidecar/go.mod}
      - uses: actions/setup-go@v5
        with: {cache-dependency-path: go.sum}
      - uses: actions/setup-go@v5
        with: {cache-dependency-path: sidecar/go.sum, cache: "false"}
      - uses: actions/setup-go@v5
        with: {cache-dependency-path: sidecar/go.sum}
      - uses: actions/setup-node@v4
        with: {node-version: "22"}
      - run: npm ci
  b:
    steps:
      - uses: actions/setup-node@v4
        with: {node-version: "22"}
      - run: npm audit
`
	got, err := cacheProblems("fake.yml", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("cacheProblems = %q, want 3 setup-go problems and 1 setup-node", got)
	}
	if !strings.Contains(got[3], "setup-node") || !strings.Contains(got[3], "/ a:") {
		t.Errorf("setup-node problem %q, want job a (it runs npm ci)", got[3])
	}
	if _, err := cacheProblems("bad.yml", "jobs: [unclosed"); err == nil ||
		!strings.Contains(err.Error(), "bad.yml") {
		t.Fatalf("malformed workflow: err %v, want a parse error naming the file", err)
	}
	if got, err := cacheProblems("empty.yml", ""); err != nil || len(got) != 0 {
		t.Fatalf("empty workflow: %q, %v; want no problems", got, err)
	}
}

// ci.yml and perfgate.yml (and every other workflow) restore the caches.
func TestEverySetupGoHasCacheDependencyPath(t *testing.T) {
	seen := 0
	for _, path := range workflowFiles(t) {
		body := readRepoFile(t, path)
		seen += strings.Count(body, "actions/setup-go@")
		problems, err := cacheProblems(filepath.Base(path), body)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			t.Error(p)
		}
	}
	if seen < 2 {
		t.Fatalf("found %d setup-go steps across the workflows, want ci.yml and perfgate.yml",
			seen)
	}
}

var (
	reportCount = regexp.MustCompile(`test "\$\{#reports\[@\]\}" -eq (\d+)`)
	shardLoop   = regexp.MustCompile(`for shard in ([a-z0-9 -]+); do`)
)

// benchShards returns the report directory names of the PG17 bench legs:
// since ci-03b each shard of the bench job's pg x shard matrix writes
// pgincidentbench/<shard> (the steps of the test job did before).
func benchShards(t *testing.T) []string {
	t.Helper()
	bench := mxJobNamed(t, loadMatrixJobs(t), "bench")
	steps := mxSteps(bench, "TestPGIncidentBench")
	if len(steps) != 1 || !strings.HasSuffix(steps[0].Env["SAGE_BENCH_REPORT_DIR"],
		"/${{ matrix.shard.name }}") {
		t.Fatalf("bench job bench steps %v, want one writing its shard's directory", steps)
	}
	var out []string
	for _, sh := range bench.Strategy.Matrix.Shard {
		out = append(out, sh.Name)
	}
	sort.Strings(out)
	return out
}

// stepMatch returns the first submatch of re across a job's run scripts.
func stepMatch(job pipeJob, re *regexp.Regexp) string {
	for _, st := range job.Steps {
		if m := re.FindStringSubmatch(st.Run); m != nil {
			return m[1]
		}
	}
	return ""
}

// bench-sign signs exactly one report per shard, and the release stages
// every shard's report, so a new shard cannot ship unsigned or unlisted.
func TestBenchSignCountsEveryShard(t *testing.T) {
	wf := loadCIPipeline(t)
	shards := benchShards(t)
	if len(shards) < 2 {
		t.Fatalf("bench matrix shards %q, want the bench split in shards", shards)
	}
	n, err := strconv.Atoi(stepMatch(wf.Jobs["bench-sign"], reportCount))
	if err != nil || n != len(shards) {
		t.Errorf("bench-sign expects %d reports (err %v), the bench matrix writes %d: %q",
			n, err, len(shards), shards)
	}
	staged := strings.Fields(stepMatch(wf.Jobs["release"], shardLoop))
	sort.Strings(staged)
	if !slices.Equal(staged, shards) {
		t.Errorf("release stages shards %q, the bench matrix writes %q", staged, shards)
	}
}
