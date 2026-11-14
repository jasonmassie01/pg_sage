package srebench

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// compareFlag is the replay compare mode (sre-03 Phase 2):
//
//	go test ./sre-bench/ -run TestReplayCompare -args --compare require_support_citation
//
// runs the corpus under both settings of the named investigator switch.
var compareFlag = flag.String("compare", "", "replay compare mode: "+CompareSupportCitation)

// TestReplayCompare replays the corpus with require_support_citation
// off and on (the fake model, or the live endpoint as TestReplayCorpus
// reads it), fails on any deterministic-arm drift or a failed replay
// gate under either setting, and writes the replay report with the
// per-family compare table. The kept-contest gate is reported, not
// enforced: only a live-model run decides the shipped default.
func TestReplayCompare(t *testing.T) {
	setting, err := ParseCompare(*compareFlag)
	if err != nil {
		t.Fatal(err)
	}
	if setting == "" {
		t.Skip("compare mode off: pass -args --compare " + CompareSupportCitation)
	}
	dsn := testdb.SkipUnlessLive(t)
	llmCfg, err := LLMConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	budget := 2 * replayBudget
	if llmCfg.Mode == LLMLive {
		budget += 2 * liveModelBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	env := NewEnv(ctx, t, dsn)
	cases := splitCorpus(t)
	off, on := RunSupportCompare(ctx, env, cases, llmCfg)
	for _, r := range append(append([]Result(nil), off...), on...) {
		if r.Err != nil || r.Skipped != "" {
			t.Errorf("%s %s: err %v skipped %q", r.Arm, r.Scenario.ID, r.Err, r.Skipped)
		}
	}
	cmp := BuildSupportCompare(off, on)
	names := compareArmNames()
	meta := ReplayMeta{Arms: names, Gated: names, LLM: llmCfg, GeneratedAt: time.Now().UTC()}
	offRep := BuildReplayReport(off, cases, meta)
	rep := BuildReplayReport(on, cases, meta)
	rep.Compare = &cmp
	t.Log("\n" + cmp.Markdown())
	jsonPath, mdPath, err := WriteReplayReport(ReportDir(os.Getenv(EnvReportDir),
		t.TempDir()), rep)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("replay compare report: %s, %s", jsonPath, mdPath)
	for _, p := range cmp.Parity {
		t.Errorf("deterministic-arm parity: %s", p)
	}
	checkCompareGates(t, map[string][]GateResult{"off": offRep.Gates, "on": rep.Gates})
}

// checkCompareGates fails on any replay gate either setting missed.
func checkCompareGates(t *testing.T, bySetting map[string][]GateResult) {
	t.Helper()
	for setting, gates := range bySetting {
		for _, g := range FailedGates(gates) {
			t.Errorf("require_support_citation %s: replay gate %s (%s, %s) failed: "+
				"observed %s, threshold %s", setting, g.ID, g.Family, g.Arm, g.Observed,
				g.Threshold)
		}
	}
}
