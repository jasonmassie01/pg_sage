package srebench

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Gaps found in the post-test audit: hypothesis ranking across diagnosis
// revisions, Markdown cell escaping of free text, and contamination
// retries through Run.

func TestRankHypotheses_LatestRevisionOnly(t *testing.T) {
	hs := []sre.HypothesisRecord{
		{Revision: 2, Ordinal: 0, Node: "inactive_slot", Status: sre.HypothesisRoot},
		{Revision: 2, Ordinal: 1, Node: "write_surge", Status: sre.HypothesisContributing},
		{Revision: 2, Ordinal: 2, Node: "archiver_failure", Status: sre.HypothesisRuledOut},
		{Revision: 2, Ordinal: 3, Node: "slow_consumer", Status: sre.HypothesisUnproven},
		{Revision: 1, Ordinal: 0, Node: "archiver_failure", Status: sre.HypothesisRoot},
		{Revision: 1, Ordinal: 1, Node: "write_surge", Status: sre.HypothesisContributing},
	}
	var o Outcome
	rankHypotheses(&o, hs)
	if strings.Join(o.Ranked, ",") != "inactive_slot,write_surge,slow_consumer" ||
		strings.Join(o.Contributing, ",") != "write_surge" {
		t.Fatalf("ranked %v contributing %v", o.Ranked, o.Contributing)
	}
	var empty Outcome
	rankHypotheses(&empty, nil)
	if empty.Ranked != nil || empty.Contributing != nil {
		t.Fatalf("no hypotheses: %+v", empty)
	}
}

func TestMarkdown_FreeTextStaysInItsCell(t *testing.T) {
	r := BuildReport([]Result{{Scenario: Scenario{ID: "s", Family: sre.TriggerLock,
		Class: ClassPositive, Gold: Gold{Root: "x"}}, Arm: armA, Repeat: 1, Attempts: 1,
		Err: &Unsupported{Reason: "a | b\nc"}}}, ReportMeta{Arms: []string{armA}})
	md := r.Markdown()
	if !strings.Contains(md, `error: unsupported: a \| b c |`) {
		t.Fatalf("free text broke the table:\n%s", md)
	}
}

// A contaminated premise is retried per arm, up to maxAttempts, and the
// derived arms carry the live arm's attempt count.
func TestRun_ContaminatedPremiseIsRetried(t *testing.T) {
	prog := &fakeProgram{manifestErr: &Contaminated{Reason: "foreign WAL"}}
	rs := Run(context.Background(), &Env{}, []Scenario{lockScenarioWith(prog)},
		fakeConfig(1, &fakeArm{name: "fake"}))
	if prog.injects != maxAttempts || prog.recovers != maxAttempts || len(rs) != 3 {
		t.Fatalf("program %+v, %d results", prog, len(rs))
	}
	for _, r := range rs {
		if r.Attempts != maxAttempts || scored(r) ||
			!strings.Contains(r.Err.Error(), "foreign WAL") {
			t.Fatalf("result %+v", r)
		}
	}
}
