package srebench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
)

// CI runs PGIncidentBench in shards (SAGE_BENCH_FAMILIES) so that no
// bench step exceeds about 20 minutes. This contract keeps the split
// honest: in every job that runs the bench, the shards are disjoint,
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

type ciWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Env   map[string]string `yaml:"env"`
		Steps []ciStep          `yaml:"steps"`
	} `yaml:"jobs"`
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

// benchSteps returns a job's steps that run TestPGIncidentBench.
func benchSteps(wf ciWorkflow, job string) []ciStep {
	var out []ciStep
	for _, st := range wf.Jobs[job].Steps {
		if strings.Contains(st.Run, "TestPGIncidentBench") {
			out = append(out, st)
		}
	}
	return out
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
	for _, job := range []string{"test", "integration-matrix"} {
		steps := benchSteps(wf, job)
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
	wf := readCIWorkflow(t)
	for _, job := range []string{"test", "integration-matrix"} {
		dirs := map[string]bool{}
		for _, st := range benchSteps(wf, job) {
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
			if job == "test" && (dir == "" || dirs[dir]) {
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
// shard per job (core) replays it, and no workflow or job env turns it on
// for all of them.
func TestCISplit_OneShardPerJobReplays(t *testing.T) {
	wf := readCIWorkflow(t)
	if v, set := wf.Env[EnvReplay]; set {
		t.Errorf("ci.yml sets %s=%q for every job", EnvReplay, v)
	}
	for _, job := range []string{"test", "integration-matrix"} {
		if v, set := wf.Jobs[job].Env[EnvReplay]; set {
			t.Errorf("%s sets %s=%q for every shard", job, EnvReplay, v)
		}
		var replaying []string
		for _, st := range benchSteps(wf, job) {
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
		{1, 4, fake, false, 2 * time.Minute},
		{1, 4, fake, true, 2*time.Minute + replayBudget},
		{0, 0, fake, false, 0},
		{0, 0, fake, true, replayBudget},
		{2, 3, live, false, 3*time.Minute + liveModelBudget},
		{2, 3, live, true, 3*time.Minute + replayBudget + liveModelBudget},
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
