package srebench

import (
	"testing"
	"time"
)

// ci-17: TestReplayCorpus's budget scales with its arms. No concurrency or
// database cases: replayCorpusBudget is a pure function.
func TestReplayCorpusBudget_PerArm(t *testing.T) {
	fake := LLMConfig{Mode: LLMFake}
	live := LLMConfig{Mode: LLMLive}
	cases := []struct {
		name string
		arms int
		llm  LLMConfig
		want time.Duration
	}{
		{"one arm", 1, fake, replayBudget},
		{"three arms", 3, fake, 3 * replayBudget},
		{"three arms, live model", 3, live, 3*replayBudget + liveModelBudget},
		{"zero arms still bounded", 0, fake, replayBudget},
		{"negative arms still bounded", -2, fake, replayBudget},
		{"zero-value config is not live", 2, LLMConfig{}, 2 * replayBudget},
	}
	for _, c := range cases {
		if got := replayCorpusBudget(c.arms, c.llm); got != c.want {
			t.Errorf("%s: replayCorpusBudget(%d, %s) = %s, want %s", c.name, c.arms,
				c.llm.Mode, got, c.want)
		}
	}
	if replayCorpusBudget(3, fake) != 9*time.Minute {
		t.Fatalf("three fake arms = %s, want 9m0s", replayCorpusBudget(3, fake))
	}
}
