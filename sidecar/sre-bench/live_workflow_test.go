package srebench

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Roadmap 2.4: the nightly live-model arm is the bench-live job of
// ci.yml (so its signature carries the release workflow's identity, the
// one the sidecar accepts). The contract: it runs only on the nightly
// schedule or a manual dispatch, never on a pull request or push; it
// skips with a notice when its secret is missing; its live step opts in
// (PG_SAGE_LIVE_LLM=1, SAGE_BENCH_LIVE_ARM=1) with every cap set and
// the key from a secret; it runs TestLiveModelArm only; it signs the
// report keyless on master only. No other job may opt in to live calls.

const liveSecret = "PG_SAGE_BENCH_OPENAI_API_KEY"

type liveStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
	Uses string            `yaml:"uses"`
}

type liveJob struct {
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Env         map[string]string `yaml:"env"`
	Steps       []liveStep        `yaml:"steps"`
	Timeout     int               `yaml:"timeout-minutes"`
}

type liveWorkflow struct {
	On   map[string]any     `yaml:"on"`
	Jobs map[string]liveJob `yaml:"jobs"`
}

func readLiveWorkflow(t *testing.T) (liveWorkflow, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf liveWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	return wf, string(raw)
}

func TestLiveWorkflow_RunsOnlyNightlyOrByHand(t *testing.T) {
	wf, _ := readLiveWorkflow(t)
	job, ok := wf.Jobs["bench-live"]
	if !ok {
		t.Fatal("ci.yml has no bench-live job")
	}
	if _, ok := wf.On["schedule"]; !ok {
		t.Fatal("ci.yml has no schedule trigger")
	}
	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Fatal("ci.yml has no workflow_dispatch trigger for a manual live run")
	}
	cond := strings.Join(strings.Fields(job.If), " ")
	if !strings.Contains(cond, "github.event_name == 'schedule'") ||
		!strings.Contains(cond, "github.event_name == 'workflow_dispatch'") ||
		strings.Contains(cond, "pull_request") || strings.Contains(cond, "'push'") {
		t.Fatalf("bench-live if = %q", cond)
	}
	if job.Permissions["id-token"] != "write" || job.Permissions["contents"] != "read" {
		t.Fatalf("permissions = %v", job.Permissions)
	}
	if job.Timeout <= 0 || job.Timeout > 90 {
		t.Fatalf("timeout-minutes = %d, want a bound under 90", job.Timeout)
	}
}

func liveTestStep(t *testing.T, job liveJob) liveStep {
	t.Helper()
	var found []liveStep
	for _, st := range job.Steps {
		if strings.Contains(st.Run, "TestLiveModelArm") {
			found = append(found, st)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d steps run TestLiveModelArm, want 1", len(found))
	}
	return found[0]
}

func TestLiveWorkflow_SkipsWithANoticeWithoutTheSecret(t *testing.T) {
	wf, _ := readLiveWorkflow(t)
	job := wf.Jobs["bench-live"]
	var check *liveStep
	for i, st := range job.Steps {
		if strings.Contains(st.Env["OPENAI_KEY_PRESENT"], "secrets."+liveSecret) {
			check = &job.Steps[i]
		}
	}
	if check == nil || check.ID == "" || !strings.Contains(check.Run, "::notice") ||
		!strings.Contains(check.Run, liveSecret) {
		t.Fatalf("no step checks %s and explains the skip: %+v", liveSecret, check)
	}
	gate := "steps." + check.ID + ".outputs.ready == 'true'"
	after := false
	for _, st := range job.Steps {
		if st.ID == check.ID {
			after = true
			continue
		}
		if after && st.Uses == "" && !strings.Contains(st.If, gate) {
			t.Errorf("step %q runs without the secret check (if: %q)", st.Name, st.If)
		}
	}
}

func TestLiveWorkflow_LiveStepOptsInWithEveryCap(t *testing.T) {
	wf, _ := readLiveWorkflow(t)
	st := liveTestStep(t, wf.Jobs["bench-live"])
	env := map[string]string{}
	for k, v := range wf.Jobs["bench-live"].Env {
		env[k] = v
	}
	for k, v := range st.Env {
		env[k] = v
	}
	if env[EnvLiveLLM] != "1" || env[EnvLiveArm] != "1" {
		t.Fatalf("the live step must opt in: %v", env)
	}
	if env[EnvLLMKey] != "${{ secrets."+liveSecret+" }}" {
		t.Fatalf("%s = %q, want the secret", EnvLLMKey, env[EnvLLMKey])
	}
	for _, k := range []string{EnvLLMURL, EnvLLMModel, EnvLLMRPM, EnvLLMMaxRequests,
		EnvLLMMaxTokens, EnvLLMMaxWall, EnvLLMMaxSpend, EnvLLMPriceIn, EnvLLMPriceOut,
		EnvReportDir, "SAGE_TEST_DATABASE_URL", "SAGE_BENCH_PG_SAGE_COMMIT"} {
		if strings.TrimSpace(env[k]) == "" {
			t.Errorf("the live step does not set %s", k)
		}
	}
	if !regexp.MustCompile(`-run '\^TestLiveModelArm\$'`).MatchString(st.Run) ||
		strings.Contains(st.Run, "./...") {
		t.Fatalf("the live step must run TestLiveModelArm alone: %q", st.Run)
	}
}

func TestLiveWorkflow_SignsOnMasterOnly(t *testing.T) {
	wf, _ := readLiveWorkflow(t)
	var sign *liveStep
	for i, st := range wf.Jobs["bench-live"].Steps {
		if strings.Contains(st.Run, "cosign sign-blob") {
			sign = &wf.Jobs["bench-live"].Steps[i]
		}
	}
	if sign == nil || !strings.Contains(sign.If, "github.ref == 'refs/heads/master'") ||
		!strings.Contains(sign.Run, "bench verify") {
		t.Fatalf("signing step = %+v", sign)
	}
}

func TestLiveWorkflow_NoOtherJobCallsALiveModel(t *testing.T) {
	wf, _ := readLiveWorkflow(t)
	for name, job := range wf.Jobs {
		if name == "bench-live" {
			continue
		}
		envs := []map[string]string{job.Env}
		for _, st := range job.Steps {
			envs = append(envs, st.Env)
			if strings.Contains(st.Run, "TestLiveModelArm") {
				t.Errorf("job %s runs the live arm", name)
			}
		}
		for _, e := range envs {
			for _, k := range []string{EnvLiveLLM, EnvLiveArm, EnvLLMURL, EnvLLMKey} {
				if _, ok := e[k]; ok {
					t.Errorf("job %s sets %s: live model calls only run in bench-live", name, k)
				}
			}
			for k, v := range e {
				if strings.Contains(v, "secrets."+liveSecret) {
					t.Errorf("job %s reads the live key into %s", name, k)
				}
			}
		}
	}
}
