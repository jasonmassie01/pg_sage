package srebench

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// The replay corpus's tuning / held-out split (roadmap 2.4,
// replay/split.go). The quality gates and the model lift read held-out
// cases only; the safety gates read every case. SAGE_BENCH_SPLIT selects
// which cases a replay runs: all (default), held_out, or tuning (for
// iterating on thresholds without looking at the held-out results).

// EnvSplit selects the replayed cases.
const EnvSplit = "SAGE_BENCH_SPLIT"

// SplitAll marks a gate that read every case.
const SplitAll = replay.SplitAll

// ParseSplit reads EnvSplit: empty means all.
func ParseSplit(v string) (string, error) {
	switch v = strings.TrimSpace(v); v {
	case "", replay.SplitAll:
		return replay.SplitAll, nil
	case replay.SplitHeldOut, replay.SplitTuning:
		return v, nil
	}
	return "", fmt.Errorf("%s=%q: want %s, %s or %s", EnvSplit, v, replay.SplitAll,
		replay.SplitHeldOut, replay.SplitTuning)
}

// heldOutOnly keeps the held-out replay runs.
func heldOutOnly(rs []Result) []Result {
	out := make([]Result, 0, len(rs))
	for _, r := range rs {
		if r.Scenario.Split == replay.SplitHeldOut {
			out = append(out, r)
		}
	}
	return out
}

// heldOutGate marks a quality gate as read on the held-out cases.
func heldOutGate(g GateResult) GateResult {
	g.Split = replay.SplitHeldOut
	if g.Status == GateNotEvaluated && g.Reason != fakeModelReason {
		g.Reason += " among the held-out cases"
	}
	return g
}

// everyCaseGate marks a safety gate as read on every case.
func everyCaseGate(g GateResult) GateResult {
	g.Split = SplitAll
	return g
}
