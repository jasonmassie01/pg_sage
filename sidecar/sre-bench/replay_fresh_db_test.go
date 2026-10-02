package srebench

import (
	"context"
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Fresh replay cases live outside the R1 corpus (its 10/5/5 class mix is
// fixed). They were authored after a fix that a corpus case motivated,
// as a check of the same shape with different numbers.
const freshCasesDir = "testdata/replay-fresh"

// TestReplayFreshCases replays the fresh cases through the causal graph
// and requires the gold root, with no errors or skips.
func TestReplayFreshCases(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	cases, err := replay.Load(os.DirFS(freshCasesDir), probes.Catalog())
	if err != nil {
		t.Fatalf("fresh cases: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), replayBudget)
	t.Cleanup(cancel)
	rs := RunReplay(ctx, NewEnv(ctx, t, dsn), cases, []LiveArm{CausalGraph{}})
	if len(rs) != len(cases) {
		t.Fatalf("%d results for %d cases", len(rs), len(cases))
	}
	for _, r := range rs {
		if r.Err != nil || r.Skipped != "" {
			t.Errorf("%s: err %v skipped %q", r.Scenario.ID, r.Err, r.Skipped)
			continue
		}
		if r.Outcome.Root != r.Scenario.Gold.Root {
			t.Errorf("%s: root %q, want %q (contributing %v)", r.Scenario.ID,
				r.Outcome.Root, r.Scenario.Gold.Root, r.Outcome.Contributing)
		}
	}
}
