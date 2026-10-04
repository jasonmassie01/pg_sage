package srebench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Contract (owner decision 2026-10-04, PR #116): bench-live measures the
// tool-calling investigator inside the existing caps. A step with id
// "arm" picks one model arm per run and the live step reads it as
// SAGE_BENCH_LIVE_MODEL_ARM: a v* tag measures the review arm (the
// release's root authority reads its lift); scheduled and manual runs
// alternate by UTC day of the year. The caps' defaults do not change.

func armStep(t *testing.T, job liveJob) (liveStep, int) {
	t.Helper()
	for i, st := range job.Steps {
		if st.ID == "arm" {
			return st, i
		}
	}
	t.Fatal("bench-live has no step with id arm")
	return liveStep{}, -1
}

func TestLiveWorkflow_PicksOneModelArmBeforeTheLiveStep(t *testing.T) {
	wf, _ := readLiveWorkflow(t)
	job := wf.Jobs["bench-live"]
	arm, at := armStep(t, job)
	live := liveTestStep(t, job)
	liveAt := -1
	for i, st := range job.Steps {
		if st.Name == live.Name {
			liveAt = i
		}
	}
	if at < 0 || liveAt < 0 || at > liveAt {
		t.Fatalf("arm step at %d, live step at %d: the arm must be picked first", at, liveAt)
	}
	if !strings.Contains(arm.If, "steps.secret.outputs.ready == 'true'") {
		t.Fatalf("the arm step must skip without the secret: %q", arm.If)
	}
	if got := live.Env[EnvLiveModelArm]; got != "${{ steps.arm.outputs.arm }}" {
		t.Fatalf("live step %s = %q, want the arm step's output", EnvLiveModelArm, got)
	}
	for k, def := range map[string]string{EnvLLMMaxRequests: "'400'",
		EnvLLMMaxTokens: "'2500000'", EnvLLMMaxWall: "'45m'", EnvLLMMaxSpend: "'2'"} {
		if !strings.Contains(live.Env[k], "|| "+def+" }}") {
			t.Errorf("%s = %q: the cap default must stay %s", k, live.Env[k], def)
		}
	}
}

// runArmStep runs the arm step's script with a ref and a day of the year
// and returns the arm it wrote to GITHUB_OUTPUT.
func runArmStep(t *testing.T, script, ref, day string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the arm step is a bash script and bash is missing: %v", err)
	}
	out := filepath.Join(t.TempDir(), "output")
	cmd := exec.Command(bash, "-euo", "pipefail", "-c", script)
	cmd.Env = append(os.Environ(), "GITHUB_REF="+ref, "GITHUB_OUTPUT="+out,
		"BENCH_DAY_OF_YEAR="+day)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("arm step (%s, day %s): %v: %s", ref, day, err, msg)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "arm="); ok {
			return v
		}
	}
	t.Fatalf("arm step wrote no arm: %q", raw)
	return ""
}

func TestLiveWorkflow_ArmAlternatesByNightAndTagsMeasureTheReviewArm(t *testing.T) {
	wf, _ := readLiveWorkflow(t)
	arm, _ := armStep(t, wf.Jobs["bench-live"])
	cases := []struct{ ref, day, want string }{
		{"refs/heads/master", "002", ArmLLM},
		{"refs/heads/master", "001", ArmInvestigator},
		{"refs/heads/master", "008", ArmLLM},
		{"refs/heads/master", "009", ArmInvestigator},
		{"refs/heads/master", "365", ArmInvestigator},
		{"refs/tags/v1.10.1", "001", ArmLLM},
		{"refs/tags/v1.10.1", "002", ArmLLM},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		got := runArmStep(t, arm.Run, c.ref, c.day)
		if got != c.want {
			t.Errorf("%s day %s: arm %q, want %q", c.ref, c.day, got, c.want)
		}
		seen[got] = true
	}
	if !seen[ArmLLM] || !seen[ArmInvestigator] {
		t.Fatalf("the nightly must measure both model arms over two nights: %v", seen)
	}
}
