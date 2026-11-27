package firstlook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: add, checkFor and CountChecks touch only the
// pass they are given, and a pass is never shared between goroutines.

func invalidIndexes(n int) []Index {
	var idx []Index
	for i := 1; i <= n; i++ {
		x := btree(uint32(i), uint32(100+i), "t", 1)
		x.Valid = false
		idx = append(idx, x)
	}
	return idx
}

func capPass(maxItems int) *pass {
	th := DefaultThresholds()
	th.MaxItemsPerRule = maxItems
	return &pass{opts: Options{Thresholds: th, Now: time.Now}, report: &Report{}}
}

func TestAddTruncatedOnlyPastTheCap(t *testing.T) {
	p := capPass(3)
	at := checkFor(p.add(RuleInvalidIndex,
		InvalidIndexes(invalidIndexes(3), IndexBuilds{})), "")
	if at.Truncated || at.Note != "" || at.Status != CheckFinding {
		t.Fatalf("at the cap: check = %+v, want a finding that is not truncated", at)
	}
	over := checkFor(p.add(RuleInvalidIndex,
		InvalidIndexes(invalidIndexes(4), IndexBuilds{})), "")
	if !over.Truncated || over.Note != "1 more not shown" || over.Status != CheckFinding {
		t.Fatalf("cap plus one: check = %+v, want truncated with 1 more not shown", over)
	}
	if len(p.report.Items) != 6 {
		t.Fatalf("kept %d items, want 3 + 3", len(p.report.Items))
	}
}

func TestAddFiftyMoreNotShown(t *testing.T) {
	p := capPass(50)
	c := checkFor(p.add(RuleInvalidIndex,
		InvalidIndexes(invalidIndexes(100), IndexBuilds{})), "")
	if !c.Truncated || c.Note != "50 more not shown" {
		t.Fatalf("check = %+v, want truncated with 50 more not shown", c)
	}
}

func TestAddNoItemsIsOKAndNotTruncated(t *testing.T) {
	c := checkFor(capPass(50).add(RuleInvalidIndex, nil), "")
	if c.Truncated || c.Status != CheckOK || c.Rule != RuleInvalidIndex {
		t.Fatalf("nil items: check = %+v", c)
	}
}

func TestCheckForDegradedNoteAndSection(t *testing.T) {
	c := checkFor(outcome{rule: RuleSequenceRunway, note: degradedPrefix + "GRANT SELECT"},
		"agent_posture")
	if c.Status != CheckDegraded || c.Note != "GRANT SELECT" || c.Section != "agent_posture" {
		t.Fatalf("check = %+v, want degraded with the prefix stripped", c)
	}
}

func TestEvalIndexesMarksEveryRuleTruncatedWhenTheReadWasCut(t *testing.T) {
	p := capPass(50)
	p.window = StatsWindow{Known: true, Since: time.Now().Add(-90 * 24 * time.Hour)}
	for _, cut := range []bool{false, true} {
		for _, o := range p.evalIndexes(invalidIndexes(1), nil, IndexBuilds{}, cut) {
			c := checkFor(o, "")
			if c.Truncated != cut {
				t.Fatalf("read cut=%v: check %+v", cut, c)
			}
			if cut && !strings.Contains(c.Note, "only the first") {
				t.Fatalf("read cut: note %q does not say the read was cut", c.Note)
			}
		}
	}
}

func TestCountChecksBySection(t *testing.T) {
	checks := []Check{
		{Rule: RuleInvalidIndex, Status: CheckOK},
		{Rule: RuleDuplicateIndex, Status: CheckFinding, Truncated: true},
		{Rule: RuleSequenceRunway, Status: CheckDegraded, Note: "GRANT SELECT"},
		{Rule: RuleXIDRunway, Status: CheckDegraded, Truncated: true},
		{Rule: "AP-01", Section: SectionAgentPosture, Status: CheckDegraded},
	}
	got := CountChecks(checks, "")
	if got != (CheckCounts{Total: 4, Degraded: 2, Truncated: 2}) {
		t.Fatalf("catalog counts = %+v", got)
	}
	if got := CountChecks(checks, SectionAgentPosture); got != (CheckCounts{Total: 1,
		Degraded: 1}) {
		t.Fatalf("posture counts = %+v", got)
	}
	if got := CountChecks(nil, ""); got != (CheckCounts{}) {
		t.Fatalf("nil checks = %+v, want zero", got)
	}
}

func TestCheckTruncatedJSON(t *testing.T) {
	b, err := json.Marshal(Check{Rule: RuleInvalidIndex, Status: CheckFinding, Truncated: true})
	if err != nil || !strings.Contains(string(b), `"truncated":true`) {
		t.Fatalf("truncated check JSON = %s, %v", b, err)
	}
	b, err = json.Marshal(Check{Rule: RuleInvalidIndex, Status: CheckOK})
	if err != nil || strings.Contains(string(b), "truncated") {
		t.Fatalf("untruncated check JSON = %s, %v, want the field omitted", b, err)
	}
}
