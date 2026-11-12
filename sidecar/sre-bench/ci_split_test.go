package srebench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
)

// CI runs PGIncidentBench in shards (SAGE_BENCH_FAMILIES) so that no
// bench step exceeds about 20 minutes. Since ci-03b each shard is a leg
// of the bench job's pg x shard matrix; the contract expands the matrix
// the way GitHub does and checks every major's legs as one set (one
// "job" per major below). This contract keeps the split
// honest: on every major, the shards are disjoint,
// together cover every bench family (a new family cannot be forgotten),
// each shard is gated by SAGE_BENCH_RUN=1 and runs under a 2400 s go test
// timeout that its own bench budget fits in, and each writes its report
// to its own directory.

// benchTimeout is the go test -timeout of every bench step.
const benchTimeout = 2400 * time.Second

type ciStep struct {
	Name           string            `yaml:"name"`
	Env            map[string]string `yaml:"env"`
	Run            string            `yaml:"run"`
	TimeoutMinutes int               `yaml:"timeout-minutes"`
}

type ciJob struct {
	Env      map[string]string `yaml:"env"`
	Strategy struct {
		Matrix struct {
			PG    []string            `yaml:"pg"`
			Shard []map[string]string `yaml:"shard"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []ciStep `yaml:"steps"`
}

type ciWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]ciJob  `yaml:"jobs"`
}

func readCIWorkflow(t *testing.T) ciWorkflow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	return wf
}

// benchJob is the job whose matrix legs run the bench shards.
const benchJob = "bench"

// matrixRef is a whole ${{ matrix.<key> }} expression.
var matrixRef = regexp.MustCompile(`\$\{\{\s*matrix\.([a-z_.]+)\s*\}\}`)

// expandMatrix substitutes every whole ${{ matrix.<key> }} in v with the
// leg's value ("" for a key the leg lacks, as GitHub does). matrix values
// inside a larger expression are left as they are.
func expandMatrix(v string, leg map[string]string) string {
	return matrixRef.ReplaceAllStringFunc(v, func(ref string) string {
		return leg[matrixRef.FindStringSubmatch(ref)[1]]
	})
}

// legEnv is a step's env for one leg. A variable that is one matrix value
// the leg lacks is dropped: GitHub sets it empty, which the bench reads
// as unset.
func legEnv(env, leg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		got := expandMatrix(v, leg)
		whole := strings.TrimSpace(v)
		if got == "" && matrixRef.FindString(whole) == whole {
			continue
		}
		out[k] = got
	}
	return out
}

// benchLegs expands the bench job's matrix: for every major ("bench
// pgNN"), one step per shard, with the leg's matrix values substituted.
func benchLegs(wf ciWorkflow) map[string][]ciStep {
	job := wf.Jobs[benchJob]
	out := map[string][]ciStep{}
	for _, pg := range job.Strategy.Matrix.PG {
		for _, shard := range job.Strategy.Matrix.Shard {
			leg := map[string]string{"pg": pg}
			for k, v := range shard {
				leg["shard."+k] = v
			}
			for _, st := range job.Steps {
				if !strings.Contains(st.Run, "TestPGIncidentBench") {
					continue
				}
				st.Name = expandMatrix(st.Name, leg)
				st.Run = expandMatrix(st.Run, leg)
				st.Env = legEnv(st.Env, leg)
				name := benchJob + " pg" + pg
				out[name] = append(out[name], st)
			}
		}
	}
	return out
}

func sortedLegs(legs map[string][]ciStep) []string {
	var out []string
	for name := range legs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestExpandMatrix(t *testing.T) {
	leg := map[string]string{"pg": "17", "shard.name": "core-a"}
	for in, want := range map[string]string{
		"x/${{ matrix.shard.name }}":              "x/core-a",
		"${{matrix.pg}}-${{ matrix.shard.name }}": "17-core-a",
		"${{ matrix.shard.replay }}":              "",
		"${{ matrix.pg == 17 && '10' || '' }}":    "${{ matrix.pg == 17 && '10' || '' }}",
		"plain":                                   "plain",
	} {
		if got := expandMatrix(in, leg); got != want {
			t.Errorf("expandMatrix(%q) = %q, want %q", in, got, want)
		}
	}
	env := legEnv(map[string]string{"A": "${{ matrix.shard.replay }}", "B": "1",
		"C": "${{ matrix.pg }}"}, leg)
	if _, set := env["A"]; set || env["B"] != "1" || env["C"] != "17" || len(env) != 2 {
		t.Fatalf("legEnv = %v, want A dropped, B=1, C=17", env)
	}
	wf := ciWorkflow{Jobs: map[string]ciJob{}}
	if got := benchLegs(wf); len(got) != 0 {
		t.Fatalf("benchLegs of a workflow without a bench job = %v, want none", got)
	}
}

func allBenchFamilies() []string {
	seen := map[sre.TriggerKind]bool{}
	for _, sc := range Scenarios() {
		seen[sc.Family] = true
	}
	var out []string
	for f := range seen {
		out = append(out, string(f))
	}
	sort.Strings(out)
	return out
}

func TestCISplit_ShardsCoverEveryFamilyOnce(t *testing.T) {
	wf := readCIWorkflow(t)
	all := allBenchFamilies()
	legs := benchLegs(wf)
	if got := sortedLegs(legs); len(got) != 5 {
		t.Fatalf("bench legs by major %q, want PostgreSQL 14-18", got)
	}
	for _, job := range sortedLegs(legs) {
		steps := legs[job]
		if len(steps) < 2 {
			t.Fatalf("%s: %d bench steps, want the bench split in shards", job, len(steps))
		}
		owner := map[string]string{}
		for _, st := range steps {
			fams, err := ParseFamilies(st.Env[EnvFamilies])
			if err != nil || len(fams) == 0 {
				t.Errorf("%s / %s: %s=%q (err %v), want a family list", job, st.Name,
					EnvFamilies, st.Env[EnvFamilies], err)
			}
			for _, f := range fams {
				if prev, dup := owner[string(f)]; dup {
					t.Errorf("%s: family %s runs in %q and %q", job, f, prev, st.Name)
				}
				owner[string(f)] = st.Name
			}
		}
		for _, f := range all {
			if owner[f] == "" {
				t.Errorf("%s: bench family %s is in no shard", job, f)
			}
		}
	}
}

func TestCISplit_EveryShardIsGatedAndBounded(t *testing.T) {
	legs := benchLegs(readCIWorkflow(t))
	if len(legs) == 0 {
		t.Fatal("ci.yml has no bench legs")
	}
	for _, job := range sortedLegs(legs) {
		dirs := map[string]bool{}
		for _, st := range legs[job] {
			if !BenchRunRequested(func(k string) string { return st.Env[k] }) {
				t.Errorf("%s / %s does not set %s=1", job, st.Name, EnvRun)
			}
			if !strings.Contains(st.Run, "-timeout 2400s") {
				t.Errorf("%s / %s: go test timeout is not 2400s", job, st.Name)
			}
			if st.TimeoutMinutes < 40 || st.TimeoutMinutes > 45 {
				t.Errorf("%s / %s: timeout-minutes = %d, want 40-45", job, st.Name,
					st.TimeoutMinutes)
			}
			dir := st.Env[EnvReportDir]
			if dir == "" || dirs[dir] {
				t.Errorf("%s / %s: report dir %q is missing or shared", job, st.Name, dir)
			}
			dirs[dir] = true
			fams, _ := ParseFamilies(st.Env[EnvFamilies])
			n := len(FilterScenarios(Scenarios(), fams))
			replay := BenchReplayRequested(func(k string) string { return st.Env[k] })
			if b := benchBudget(1, n, LLMConfig{Mode: LLMFake}, replay); b >= benchTimeout {
				t.Errorf("%s / %s: bench budget %s for %d scenarios is not under %s",
					job, st.Name, b, n, benchTimeout)
			}
		}
	}
}

// benches-11: the replay corpus is the same in every shard, so exactly one
// shard per major (core part A) replays it, and no workflow or job env
// turns it on for all of them.
func TestCISplit_OneShardPerJobReplays(t *testing.T) {
	wf := readCIWorkflow(t)
	if v, set := wf.Env[EnvReplay]; set {
		t.Errorf("ci.yml sets %s=%q for every job", EnvReplay, v)
	}
	if v, set := wf.Jobs[benchJob].Env[EnvReplay]; set {
		t.Errorf("%s sets %s=%q for every shard", benchJob, EnvReplay, v)
	}
	legs := benchLegs(wf)
	if len(legs) == 0 {
		t.Fatal("ci.yml has no bench legs")
	}
	for _, job := range sortedLegs(legs) {
		var replaying []string
		for _, st := range legs[job] {
			if v, set := st.Env[EnvReplay]; set && strings.TrimSpace(v) != "1" {
				t.Errorf("%s / %s: %s=%q, want 1 or unset", job, st.Name, EnvReplay, v)
			}
			if BenchReplayRequested(func(k string) string { return st.Env[k] }) {
				replaying = append(replaying, st.Name)
			}
		}
		if len(replaying) != 1 || !strings.Contains(replaying[0], "core") {
			t.Errorf("%s: shards replaying the corpus = %q, want the core shard only",
				job, replaying)
		}
	}
}

func TestBenchBudget_ReplayOnlyWhenAsked(t *testing.T) {
	fake, live := LLMConfig{Mode: LLMFake}, LLMConfig{Mode: LLMLive}
	for _, c := range []struct {
		repeats, scenarios int
		llm                LLMConfig
		replay             bool
		want               time.Duration
	}{
		{1, 4, fake, false, 2*time.Minute + harnessBudget},
		{1, 4, fake, true, 2*time.Minute + harnessBudget + replayBudget},
		{0, 0, fake, false, harnessBudget},
		{0, 0, fake, true, harnessBudget + replayBudget},
		{2, 3, live, false, 3*time.Minute + harnessBudget + liveModelBudget},
		{2, 3, live, true, 3*time.Minute + harnessBudget + replayBudget + liveModelBudget},
	} {
		if got := benchBudget(c.repeats, c.scenarios, c.llm, c.replay); got != c.want {
			t.Errorf("benchBudget(%d, %d, %s, %v) = %s, want %s", c.repeats, c.scenarios,
				c.llm.Mode, c.replay, got, c.want)
		}
	}
}

func TestBenchReplayRequested(t *testing.T) {
	for v, want := range map[string]bool{"1": true, " 1 ": true, "": false, "0": false,
		"true": false, "yes": false} {
		got := BenchReplayRequested(func(k string) string {
			if k == EnvReplay {
				return v
			}
			return "1"
		})
		if got != want {
			t.Errorf("%s=%q: replay requested = %v, want %v", EnvReplay, v, got, want)
		}
	}
}

func TestBenchGates_ReplayGatesOnlyWithAReplay(t *testing.T) {
	shard := Report{Gates: []GateResult{{ID: GateTop1, Arm: ArmCausalGraph}}}
	if got := benchGates(shard); len(got) != 1 || got[0].ID != GateTop1 {
		t.Fatalf("a shard without replay: gates = %+v", got)
	}
	if got := benchGates(Report{}); len(got) != 0 {
		t.Fatalf("an empty report: gates = %+v", got)
	}
	core := shard
	core.Replay = &ReplayReport{Gates: []GateResult{{ID: GateForbidden, Arm: ArmLLM}}}
	got := benchGates(core)
	if len(got) != 2 || got[0].ID != GateTop1 || got[1].ID != GateForbidden ||
		got[1].Arm != ArmLLM {
		t.Fatalf("a shard with replay: gates = %+v", got)
	}
	if len(core.Gates) != 1 {
		t.Fatalf("benchGates changed the report's gates: %+v", core.Gates)
	}
}

func TestShardReportWithoutReplayIsIngested(t *testing.T) {
	rate := 1.0
	r := Report{Schema: ReportSchema, SchemaRevision: ReportSchemaRevision,
		GeneratedAt: time.Now().UTC().Add(-time.Minute), Gated: []string{ArmCausalGraph},
		Cells: []CellRecord{{Arm: ArmCausalGraph, Family: "checkpoint_storm", Runs: 12,
			SafePass: Metric{K: 12, N: 12}, Top1: Metric{K: 11, N: 12},
			MechanismPrecision: &rate}}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"replay"`) || strings.Contains(string(raw), "model_lift") {
		t.Fatalf("a shard without replay wrote a replay section: %s", raw)
	}
	run, err := earned.ParseBenchReport(raw, time.Now())
	if err != nil {
		t.Fatalf("the ledger refused a shard report without replay: %v", err)
	}
	if run.ModelLift != nil || len(run.Cells) != 1 || run.Cells[0].Family != "checkpoint_storm" ||
		run.Cells[0].Top1.K != 11 {
		t.Fatalf("ingested shard = %+v (lift %+v)", run.Cells, run.ModelLift)
	}
}

// quotedInt matches the integer literals of a workflow expression.
var quotedInt = regexp.MustCompile(`'([^']*)'`)

// worstCaseLiteral is the largest integer a workflow value can take: a
// literal, or every quoted literal of a ${{ }} expression (a v* tag picks
// the larger one). parse reads one literal; empty literals are allowed
// only when allowEmpty (an unset bar).
func worstCaseLiteral(v, env string, allowEmpty bool,
	parse func(string) (int, error)) (int, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "${{") {
		return parse(v)
	}
	worst := 0
	for _, m := range quotedInt.FindAllStringSubmatch(v, -1) {
		if strings.HasPrefix(m[1], "refs/") || (allowEmpty && m[1] == "") {
			continue // a ref the expression compares, or the unset value
		}
		n, err := parse(m[1])
		if err != nil || m[1] == "" {
			return 0, fmt.Errorf("%s: literal %q is not a count: %v", v, m[1], err)
		}
		worst = max(worst, n)
	}
	if worst == 0 {
		return 0, fmt.Errorf("%s=%q names no count", env, v)
	}
	return worst, nil
}

// worstCaseRepeats is the largest repeat count a SAGE_BENCH_REPEATS value
// can take.
func worstCaseRepeats(v string) (int, error) {
	return worstCaseLiteral(v, EnvRepeats, false, ParseRepeats)
}

func TestWorstCaseRepeats(t *testing.T) {
	ok := map[string]int{"": 1, "1": 1, " 4 ": 4,
		"${{ startsWith(github.ref, 'refs/tags/v') && '4' || '1' }}": 4,
		"${{ github.event_name == 'schedule' && '2' || '3' }}":       0}
	for v, want := range ok {
		got, err := worstCaseRepeats(v)
		if want == 0 {
			if err == nil {
				t.Errorf("worstCaseRepeats(%q) = %d, want an error (schedule is no count)",
					v, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("worstCaseRepeats(%q) = %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"0", "-1", "four", "${{ true }}",
		"${{ startsWith(github.ref, 'refs/tags/v') && 'x' || '1' }}",
		fmt.Sprintf("${{ '%d' }}", MaxRepeats+1)} {
		if n, err := worstCaseRepeats(v); err == nil {
			t.Errorf("worstCaseRepeats(%q) = %d, want an error", v, n)
		}
	}
}

// The bench budget's edge at 4 repeats, with the fixed harness allowance
// every shard has: a shard that replays the corpus (core part A,
// benches-11) fits 16 fake-model scenarios in the 2400 s step and not 17;
// a shard without the replay fits 18 and not 19.
func TestBenchBudgetEdgeAtFourRepeats(t *testing.T) {
	fake := LLMConfig{Mode: LLMFake}
	for _, c := range []struct {
		scenarios int
		replay    bool
		fits      bool
	}{{16, true, true}, {17, true, false}, {18, false, true}, {19, false, false}} {
		if b := benchBudget(4, c.scenarios, fake, c.replay); (b < benchTimeout) != c.fits {
			t.Errorf("benchBudget(4, %d, replay=%v) = %s: fits under %s = %v, want %v",
				c.scenarios, c.replay, b, benchTimeout, b < benchTimeout, c.fits)
		}
	}
	if b := benchBudget(4, 18, fake, false); b != 2340*time.Second {
		t.Fatalf("benchBudget(4, 18) = %s, want 39m0s", b)
	}
	if b := benchBudget(4, 0, fake, true); b != harnessBudget+replayBudget {
		t.Fatalf("benchBudget(4, 0, replay) = %s, want the harness and replay budgets %s",
			b, harnessBudget+replayBudget)
	}
}

// shardRepeats is a shard's worst-case repeat count: its fixed
// SAGE_BENCH_REPEATS, else what a tag's SAGE_BENCH_MIN_RUNS_PER_FAMILY bar
// sizes for the shard's scenarios (RepeatsFor, benches-06).
func shardRepeats(env map[string]string, scenarios []Scenario) (int, error) {
	if v, set := env[EnvRepeats]; set {
		return worstCaseRepeats(v)
	}
	v, set := env[EnvMinRunsPerFamily]
	if !set {
		return 1, nil
	}
	bar, err := worstCaseLiteral(v, EnvMinRunsPerFamily, true, ParseMinRuns)
	if err != nil {
		return 0, err
	}
	return RepeatsFor(scenarios, bar), nil
}

// A v* tag sets SAGE_BENCH_MIN_RUNS_PER_FAMILY=10 (the promotion bar's n
// >= 10), so every shard must fit its 2400 s timeout at the repeats that
// bar sizes, plus the replay budget where it replays, not only at the
// single pass of a pull request (ci-03).
func TestBenchShardsFitTheirTimeoutOnTags(t *testing.T) {
	wf := readCIWorkflow(t)
	legs := benchLegs(wf)
	if len(legs) == 0 {
		t.Fatal("ci.yml has no bench legs")
	}
	for _, job := range sortedLegs(legs) {
		for _, st := range legs[job] {
			env := map[string]string{}
			for k, v := range wf.Jobs[benchJob].Env {
				env[k] = v
			}
			for k, v := range st.Env {
				env[k] = v
			}
			fams, _ := ParseFamilies(env[EnvFamilies])
			scenarios := FilterScenarios(Scenarios(), fams)
			repeats, err := shardRepeats(env, scenarios)
			if err != nil {
				t.Errorf("%s / %s: %v", job, st.Name, err)
				continue
			}
			replay := BenchReplayRequested(func(k string) string { return env[k] })
			b := benchBudget(repeats, len(scenarios), LLMConfig{Mode: LLMFake}, replay)
			if b >= benchTimeout {
				t.Errorf("%s / %s: %d scenarios x %d repeats need %s, not under %s",
					job, st.Name, len(scenarios), repeats, b, benchTimeout)
			}
		}
	}
}
