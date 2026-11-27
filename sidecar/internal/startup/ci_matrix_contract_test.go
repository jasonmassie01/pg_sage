package startup

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ci-03b: the benches and the performance gate leave the suite jobs. The
// bench is a pg x shard matrix job, AgentSafetyBench a job per major and
// the gate a job with a clean server of its own, so the PR critical path is
// the slowest job, not the sum of every bench. Release, the tag image and
// :edge still wait for all of them, and the signing jobs sign only what
// the bench jobs wrote. -shuffle=on runs nightly, reported, never gating.
//
// No PostgreSQL or concurrent-access surface: these tests read ci.yml.
// The critical path itself is measured on real runs (CHECK-01, MANUAL).

// benchShardNames are the bench matrix's shards; each fits its 2400 s step
// at a tag's repeats (sre-bench/ci_split_test.go).
var benchShardNames = []string{"core-a", "core-b", "reactive-a", "reactive-b",
	"runway-a", "runway-b"}

type mxShard struct {
	Name     string `yaml:"name"`
	Families string `yaml:"families"`
}

type mxJob struct {
	Needs           any    `yaml:"needs"`
	If              string `yaml:"if"`
	ContinueOnError any    `yaml:"continue-on-error"`
	Strategy        struct {
		FailFast *bool `yaml:"fail-fast"`
		Matrix   struct {
			PG    []string  `yaml:"pg"`
			Shard []mxShard `yaml:"shard"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Services struct {
		Postgres struct {
			Image string `yaml:"image"`
		} `yaml:"postgres"`
	} `yaml:"services"`
	Steps []relStep `yaml:"steps"`
}

func (j mxJob) needs() []string { return ciJob{Needs: j.Needs}.needs() }

func loadMatrixJobs(t *testing.T) map[string]mxJob {
	t.Helper()
	var wf struct {
		Jobs map[string]mxJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(ciWorkflowText(t)), &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	return wf.Jobs
}

func mxJobNamed(t *testing.T, jobs map[string]mxJob, name string) mxJob {
	t.Helper()
	job, ok := jobs[name]
	if !ok {
		t.Fatalf("ci.yml has no %s job", name)
	}
	return job
}

// mxSteps returns the job's steps whose run contains needle.
func mxSteps(job mxJob, needle string) []relStep {
	var out []relStep
	for _, st := range job.Steps {
		if strings.Contains(st.Run, needle) {
			out = append(out, st)
		}
	}
	return out
}

// mxUpload returns the with: name of the job's upload-artifact steps.
func mxUploads(job mxJob) []string {
	var out []string
	for _, st := range job.Steps {
		if strings.HasPrefix(st.Uses, "actions/upload-artifact") {
			out = append(out, st.With["name"])
		}
	}
	return out
}

func assertMatrixMajors(t *testing.T, name string, job mxJob) {
	t.Helper()
	if !slices.Equal(job.Strategy.Matrix.PG, ciMajors) {
		t.Errorf("%s matrix pg = %q, want %q", name, job.Strategy.Matrix.PG, ciMajors)
	}
	if ff := job.Strategy.FailFast; ff == nil || *ff {
		t.Errorf("%s fail-fast is not false: one leg's failure would cancel the rest", name)
	}
	if !strings.HasPrefix(job.Services.Postgres.Image, ciImageRepo+":0.8.2-pg${{ matrix.pg }}-") {
		t.Errorf("%s service image %q, want the CI image of matrix.pg", name,
			job.Services.Postgres.Image)
	}
}

func TestBenchRunsAsAPgByShardMatrix(t *testing.T) {
	bench := mxJobNamed(t, loadMatrixJobs(t), "bench")
	assertMatrixMajors(t, "bench", bench)
	var names []string
	for _, sh := range bench.Strategy.Matrix.Shard {
		names = append(names, sh.Name)
		if sh.Families == "" {
			t.Errorf("bench shard %s names no families", sh.Name)
		}
	}
	if !slices.Equal(names, benchShardNames) {
		t.Errorf("bench shards %q, want %q", names, benchShardNames)
	}
	steps := mxSteps(bench, "TestPGIncidentBench")
	if len(steps) != 1 {
		t.Fatalf("bench job has %d bench steps, want one per leg", len(steps))
	}
	env := steps[0].Env
	dir := "/pgincidentbench/${{ matrix.shard.name }}"
	if env["SAGE_BENCH_FAMILIES"] != "${{ matrix.shard.families }}" ||
		!strings.HasSuffix(env["SAGE_BENCH_REPORT_DIR"], dir) {
		t.Errorf("bench step env %v, want the leg's families and report dir", env)
	}
	// Only PG17's reports are signed and shipped, so only PG17 meets the
	// promotion bar on a tag. 17 is unquoted: a quoted '17' would read as a
	// run count to the budget contract.
	bar := expr(env["SAGE_BENCH_MIN_RUNS_PER_FAMILY"])
	if !strings.Contains(bar, "matrix.pg == 17") || !strings.Contains(bar, "'10'") ||
		!strings.Contains(bar, "startsWith(github.ref, 'refs/tags/v')") {
		t.Errorf("SAGE_BENCH_MIN_RUNS_PER_FAMILY = %q, want 10 on PG17 tags only", bar)
	}
	want := "pgincidentbench-pg${{ matrix.pg }}-${{ matrix.shard.name }}"
	if ups := mxUploads(bench); !slices.Contains(ups, want) {
		t.Errorf("bench uploads %q, want %s", ups, want)
	}
}

// The suite jobs run no bench and no gate any more; each has its own job.
func TestSuitesAndBenchesRunInSeparateJobs(t *testing.T) {
	jobs := loadMatrixJobs(t)
	for _, name := range []string{"test", "integration-matrix"} {
		job := mxJobNamed(t, jobs, name)
		for _, run := range []string{"TestPGIncidentBench", "TestAgentSafetyBench",
			"TestPerfGate", "docker run"} {
			if n := len(mxSteps(job, run)); n != 0 {
				t.Errorf("%s still has %d steps running %s", name, n, run)
			}
		}
	}
	safety := mxJobNamed(t, jobs, "safety-bench")
	assertMatrixMajors(t, "safety-bench", safety)
	if len(mxSteps(safety, "TestAgentSafetyBench")) != 1 ||
		!slices.Contains(mxUploads(safety), "agentsafetybench-pg${{ matrix.pg }}") {
		t.Errorf("safety-bench: steps %d, uploads %q; want one bench step and its report",
			len(mxSteps(safety, "TestAgentSafetyBench")), mxUploads(safety))
	}
	gate := mxJobNamed(t, jobs, "perfgate")
	if gate.Services.Postgres.Image == "" || len(mxSteps(gate, "docker run")) != 0 {
		t.Errorf("perfgate: service %q, %d docker runs; want a clean service of its own",
			gate.Services.Postgres.Image, len(mxSteps(gate, "docker run")))
	}
	steps := mxSteps(gate, "TestPerfGate")
	if len(steps) != 1 ||
		!strings.Contains(steps[0].Env["PG_SAGE_PERF_SCALE"], "'schedule' && 'large'") {
		t.Errorf("perfgate: %d gate steps (%v), want one, large on the nightly", len(steps),
			steps)
	}
	if !slices.Contains(mxUploads(gate), "perfgate-pg17") {
		t.Errorf("perfgate uploads %q, want perfgate-pg17", mxUploads(gate))
	}
}

// mxDownload returns the job's download-artifact steps.
func mxDownloads(job mxJob) []relStep {
	var out []relStep
	for _, st := range job.Steps {
		if strings.HasPrefix(st.Uses, "actions/download-artifact") {
			out = append(out, st)
		}
	}
	return out
}

// State transitions: each signing job waits for the job that wrote its
// reports and for the suites, and reads every PG17 leg's report.
func TestSigningJobsWaitForTheirBenchJobs(t *testing.T) {
	jobs := loadMatrixJobs(t)
	sign := mxJobNamed(t, jobs, "bench-sign")
	if n := sign.needs(); !slices.Contains(n, "test") || !slices.Contains(n, "bench") {
		t.Errorf("bench-sign needs %v, want test and bench", n)
	}
	dl := mxDownloads(sign)
	if len(dl) != 1 || dl[0].With["pattern"] != "pgincidentbench-pg17-*" ||
		dl[0].With["merge-multiple"] != "true" || dl[0].With["name"] != "" {
		t.Errorf("bench-sign downloads %v, want pattern pgincidentbench-pg17-* merged", dl)
	}
	safety := mxJobNamed(t, jobs, "safety-bench-sign")
	if n := safety.needs(); !slices.Contains(n, "test") || !slices.Contains(n, "safety-bench") {
		t.Errorf("safety-bench-sign needs %v, want test and safety-bench", n)
	}
	dl = mxDownloads(safety)
	if len(dl) != 1 || dl[0].With["name"] != "agentsafetybench-pg17" {
		t.Errorf("safety-bench-sign downloads %v, want agentsafetybench-pg17", dl)
	}
}

// benchJobs are the jobs that left the test job. A release, the tag image
// and :edge never ship past one of them that failed (before ci-03b they
// were steps of test, which all three require).
var benchJobs = []string{"bench", "safety-bench", "perfgate"}

func TestReleaseAndDockerWaitForTheBenchJobs(t *testing.T) {
	wf := loadRelWorkflow(t)
	release := relJobNamed(t, wf, "release")
	docker := relJobNamed(t, wf, "docker")
	for _, n := range benchJobs {
		if !slices.Contains(release.needs(), n) || !slices.Contains(docker.needs(), n) {
			t.Fatalf("release needs %v, docker needs %v; both want %s", release.needs(),
				docker.needs(), n)
		}
	}
	tag := pushState("refs/tags/v2.6.0", release.needs())
	dtag := pushState("refs/tags/v2.6.0", docker.needs())
	master := pushState("refs/heads/master", docker.needs())
	assertRuns(t, "docker on master", docker.If, master, true)
	for _, n := range benchJobs {
		for _, result := range []string{"failure", "skipped", "cancelled"} {
			assertRuns(t, "release ("+n+" "+result+")", release.If, tag.with(n, result), false)
			assertRuns(t, "docker on a tag ("+n+" "+result+")", docker.If,
				dtag.with(n, result), false)
			assertRuns(t, "docker on master ("+n+" "+result+")", docker.If,
				master.with(n, result), false)
		}
	}
}

// Nightly only: the race suite with -shuffle=on -count=2 surfaces order
// dependence. It is reported (summary and artifact) and gates nothing: no
// job needs it, it may fail without failing the run, and no pull request,
// push or tag runs it.
func TestShuffleRunIsNightlyAndReportOnly(t *testing.T) {
	jobs := loadMatrixJobs(t)
	var shuffling []string
	for name, job := range jobs {
		if len(mxSteps(job, "-shuffle")) > 0 {
			shuffling = append(shuffling, name)
		}
		if slices.Contains(job.needs(), "shuffle") {
			t.Errorf("job %s needs shuffle: the shuffle run must not gate", name)
		}
	}
	if !slices.Equal(shuffling, []string{"shuffle"}) {
		t.Fatalf("jobs using -shuffle %q, want only the shuffle job", shuffling)
	}
	job := jobs["shuffle"]
	if v, ok := job.ContinueOnError.(bool); !ok || !v {
		t.Errorf("shuffle continue-on-error = %v, want true", job.ContinueOnError)
	}
	for _, event := range []string{"push", "pull_request", "workflow_dispatch"} {
		st := pushState("refs/heads/master", job.needs())
		st.ctx["github.event_name"] = event
		assertRuns(t, "shuffle on "+event, job.If, st, false)
	}
	nightly := pushState("refs/heads/master", job.needs())
	nightly.ctx["github.event_name"] = "schedule"
	assertRuns(t, "shuffle nightly", job.If, nightly, true)
	runs := wholeModuleRuns(pipeJob{Steps: pipeSteps(job.Steps)})
	if len(runs) != 1 {
		t.Fatalf("shuffle job: %d go test ./... runs, want 1", len(runs))
	}
	for _, flag := range []string{"-race", "-shuffle=on", "-count=2", "-tags=integration",
		"-json"} {
		if !slices.Contains(runs[0], flag) {
			t.Errorf("shuffle go test %q lacks %s", runs[0], flag)
		}
	}
	if len(mxSteps(job, "GITHUB_STEP_SUMMARY")) == 0 || len(mxUploads(job)) == 0 {
		t.Error("the shuffle run writes no step summary or uploads no report")
	}
}

func pipeSteps(steps []relStep) []pipeStep {
	out := make([]pipeStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, pipeStep{Name: s.Name, Uses: s.Uses, Run: s.Run, With: s.With,
			Env: s.Env})
	}
	return out
}
