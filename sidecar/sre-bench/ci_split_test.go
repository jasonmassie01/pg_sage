package srebench

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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
			if b := benchBudget(1, n, LLMConfig{Mode: LLMFake}); b >= benchTimeout {
				t.Errorf("%s / %s: bench budget %s for %d scenarios is not under %s",
					job, st.Name, b, n, benchTimeout)
			}
		}
	}
}

// quotedInt matches the integer literals of a workflow expression.
var quotedInt = regexp.MustCompile(`'([^']*)'`)

// worstCaseRepeats is the largest repeat count a SAGE_BENCH_REPEATS value
// can take: a literal, or every quoted literal of a ${{ }} expression
// (a v* tag picks the larger one).
func worstCaseRepeats(v string) (int, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "${{") {
		return ParseRepeats(v)
	}
	lits := quotedInt.FindAllStringSubmatch(v, -1)
	worst := 0
	for _, m := range lits {
		if strings.HasPrefix(m[1], "refs/") {
			continue // a ref the expression compares, not a repeat count
		}
		n, err := ParseRepeats(m[1])
		if err != nil || m[1] == "" {
			return 0, fmt.Errorf("%s: literal %q is not a repeat count: %v", v, m[1], err)
		}
		worst = max(worst, n)
	}
	if worst == 0 {
		return 0, fmt.Errorf("%s=%q names no repeat count", EnvRepeats, v)
	}
	return worst, nil
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

// The bench budget's edge at 4 repeats: 18 fake-model scenarios fit the
// 2400 s step, 19 do not.
func TestBenchBudgetEdgeAtFourRepeats(t *testing.T) {
	fake := LLMConfig{Mode: LLMFake}
	if b := benchBudget(4, 18, fake); b != 2340*time.Second || b >= benchTimeout {
		t.Fatalf("benchBudget(4, 18) = %s, want 39m0s, under %s", b, benchTimeout)
	}
	if b := benchBudget(4, 19, fake); b < benchTimeout {
		t.Fatalf("benchBudget(4, 19) = %s, want at least %s", b, benchTimeout)
	}
	if b := benchBudget(4, 0, fake); b != replayBudget {
		t.Fatalf("benchBudget(4, 0) = %s, want the replay budget %s", b, replayBudget)
	}
}

// A v* tag sets SAGE_BENCH_REPEATS=4 for the promotion bar's n >= 10, so
// every shard must fit its 2400 s timeout at its worst-case repeat count,
// not only at the single pass of a pull request.
func TestBenchShardsFitTheirTimeoutOnTags(t *testing.T) {
	wf := readCIWorkflow(t)
	for _, job := range []string{"test", "integration-matrix"} {
		for _, st := range benchSteps(wf, job) {
			v, set := st.Env[EnvRepeats]
			if !set {
				v = wf.Jobs[job].Env[EnvRepeats]
			}
			repeats, err := worstCaseRepeats(v)
			if err != nil {
				t.Errorf("%s / %s: %v", job, st.Name, err)
				continue
			}
			fams, _ := ParseFamilies(st.Env[EnvFamilies])
			n := len(FilterScenarios(Scenarios(), fams))
			if b := benchBudget(repeats, n, LLMConfig{Mode: LLMFake}); b >= benchTimeout {
				t.Errorf("%s / %s: %d scenarios x %d repeats need %s, not under %s",
					job, st.Name, n, repeats, b, benchTimeout)
			}
		}
	}
}
