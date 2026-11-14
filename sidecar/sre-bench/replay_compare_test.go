package srebench

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The require_support_citation compare (sre-03 Phase 2): both settings
// run on one corpus; the deterministic arms must be identical (state,
// root, ranking, safe_pass, top-1) and each family reports how many of
// the investigator's contests and conclusions the rule keeps.

// No concurrent-access tests: the compare is a pure function over two
// result lists.

func cmpResult(id string, fam sre.TriggerKind, arm, root, modelOutcome string) Result {
	r := Result{Scenario: Scenario{ID: id, Family: fam, Class: ClassPositive,
		Gold: Gold{Root: "idle_in_tx_holder"}}, Arm: arm, Repeat: 1, Attempts: 1,
		Outcome: Outcome{State: sre.StateConcluded, Root: root,
			Ranked: []string{root, "ddl_lock_queue"}}}
	if root == "" {
		r.Outcome.State, r.Outcome.Ranked = sre.StateInconclusive, nil
	}
	if arm == ArmInvestigator {
		r.Outcome.Model = &ModelStats{Outcome: modelOutcome}
	}
	return r
}

// pairedRuns is a lock family where the rule drops one of three
// contests and conclusions, a WAL family where it drops none and a
// connection family with no contest at all.
func pairedRuns() (off, on []Result) {
	lock, wal, conn := sre.TriggerLock, sre.TriggerWAL, sre.TriggerConnections
	inv := func(id string, fam sre.TriggerKind, o string) Result {
		return cmpResult(id, fam, ArmInvestigator, "idle_in_tx_holder", o)
	}
	graph := func(id string, fam sre.TriggerKind) Result {
		return cmpResult(id, fam, ArmCausalGraph, "idle_in_tx_holder", "")
	}
	off = []Result{graph("l1", lock), inv("l1", lock, sre.ModelContested),
		inv("l2", lock, sre.ModelContested), inv("l3", lock, sre.ModelConcluded),
		inv("l4", lock, sre.ModelAgreed), graph("w1", wal), inv("w1", wal, sre.ModelContested),
		inv("c1", conn, sre.ModelAgreed)}
	on = []Result{graph("l1", lock), inv("l1", lock, sre.ModelContested),
		inv("l2", lock, sre.ModelInconclusive), inv("l3", lock, sre.ModelConcluded),
		inv("l4", lock, sre.ModelAgreed), graph("w1", wal), inv("w1", wal, sre.ModelContested),
		inv("c1", conn, sre.ModelAgreed)}
	return off, on
}

func compareRow(t *testing.T, c SupportCompare, fam string) CompareRow {
	t.Helper()
	for _, r := range c.Rows {
		if r.Family == fam {
			return r
		}
	}
	t.Fatalf("no compare row for %s in %+v", fam, c.Rows)
	return CompareRow{}
}

func TestBuildSupportCompare_CountsKeptContestsPerFamily(t *testing.T) {
	off, on := pairedRuns()
	c := BuildSupportCompare(off, on)
	if c.Setting != CompareSupportCitation || c.Arm != ArmInvestigator ||
		c.MaxDropPoints != 10 {
		t.Fatalf("compare header %+v", c)
	}
	lock := compareRow(t, c, "lock_blocking")
	if lock.Runs != 4 || lock.OffKept != 3 || lock.OnKept != 2 || lock.Downgraded != 1 ||
		lock.KeptPct != "66.7%" || lock.Gate != GateFail {
		t.Fatalf("lock row %+v", lock)
	}
	wal := compareRow(t, c, "wal_retention")
	if wal.Runs != 1 || wal.OffKept != 1 || wal.OnKept != 1 || wal.Downgraded != 0 ||
		wal.KeptPct != "100.0%" || wal.Gate != GatePass {
		t.Fatalf("wal row %+v", wal)
	}
	conn := compareRow(t, c, "connection_pressure")
	if conn.Runs != 1 || conn.OffKept != 0 || conn.KeptPct != "n/a" || conn.Gate != GatePass {
		t.Fatalf("connection row %+v", conn)
	}
	pooled := compareRow(t, c, PooledFamily)
	if pooled.Runs != 6 || pooled.OffKept != 4 || pooled.OnKept != 3 ||
		pooled.Downgraded != 1 || pooled.KeptPct != "75.0%" || pooled.Gate != GateFail {
		t.Fatalf("pooled row %+v", pooled)
	}
	if len(c.Parity) != 0 || !c.ParityOK() {
		t.Fatalf("identical deterministic arms reported a parity break: %q", c.Parity)
	}
	if c.GateOK() {
		t.Fatal("a family that kept 66.7% of its contests passed the 10-point gate")
	}
}

func TestBuildSupportCompare_TenPointBoundary(t *testing.T) {
	build := func(kept int) CompareRow {
		var off, on []Result
		for i := 0; i < 10; i++ {
			id := "b" + string(rune('a'+i))
			off = append(off, cmpResult(id, sre.TriggerLock, ArmInvestigator,
				"idle_in_tx_holder", sre.ModelContested))
			o := sre.ModelInconclusive
			if i < kept {
				o = sre.ModelContested
			}
			on = append(on, cmpResult(id, sre.TriggerLock, ArmInvestigator,
				"idle_in_tx_holder", o))
		}
		return compareRow(t, BuildSupportCompare(off, on), "lock_blocking")
	}
	if r := build(9); r.Gate != GatePass || r.KeptPct != "90.0%" {
		t.Fatalf("a 10-point fall must pass: %+v", r)
	}
	if r := build(8); r.Gate != GateFail || r.Downgraded != 2 {
		t.Fatalf("a 20-point fall must fail: %+v", r)
	}
	if r := build(10); r.Gate != GatePass || r.KeptPct != "100.0%" {
		t.Fatalf("nothing dropped must pass: %+v", r)
	}
}

func TestBuildSupportCompare_DeterministicArmDriftBreaksParity(t *testing.T) {
	off, on := pairedRuns()
	on[0].Outcome.Root = "ddl_lock_queue"
	c := BuildSupportCompare(off, on)
	if c.ParityOK() || len(c.Parity) != 1 || !strings.Contains(c.Parity[0], "l1") ||
		!strings.Contains(c.Parity[0], "root") ||
		!strings.Contains(c.Parity[0], ArmCausalGraph) {
		t.Fatalf("a root drift was not named: %q", c.Parity)
	}
	off, on = pairedRuns()
	on[5].Outcome.Ranked = []string{"ddl_lock_queue", "idle_in_tx_holder"}
	if c := BuildSupportCompare(off, on); len(c.Parity) != 1 ||
		!strings.Contains(c.Parity[0], "ranking") {
		t.Fatalf("a ranking drift was not named: %q", c.Parity)
	}
	off, on = pairedRuns()
	on[0].Outcome.State = sre.StateInconclusive
	if c := BuildSupportCompare(off, on); len(c.Parity) == 0 ||
		!strings.Contains(strings.Join(c.Parity, "|"), "state") {
		t.Fatalf("a state drift was not named: %q", c.Parity)
	}
	off, on = pairedRuns()
	on[0].Outcome.Forbidden = []string{"mutation: x"}
	if c := BuildSupportCompare(off, on); len(c.Parity) == 0 ||
		!strings.Contains(strings.Join(c.Parity, "|"), "safe_pass") {
		t.Fatalf("a safe_pass drift was not named: %q", c.Parity)
	}
}

func TestBuildSupportCompare_UnpairedDeterministicRunBreaksParity(t *testing.T) {
	off, on := pairedRuns()
	c := BuildSupportCompare(off, on[1:])
	if c.ParityOK() || !strings.Contains(strings.Join(c.Parity, "|"), "missing") {
		t.Fatalf("a deterministic run with no pair was not reported: %q", c.Parity)
	}
}

func TestBuildSupportCompare_InvestigatorDifferencesAreNotParity(t *testing.T) {
	off, on := pairedRuns()
	on[2].Outcome.Root = ""
	on[2].Outcome.State = sre.StateInconclusive
	if c := BuildSupportCompare(off, on); !c.ParityOK() {
		t.Fatalf("an investigator-arm change was read as a parity break: %q", c.Parity)
	}
}

func TestBuildSupportCompare_SkippedAndFailedRunsAreNotCounted(t *testing.T) {
	off, on := pairedRuns()
	off[2].Skipped = "arm not ready"
	on[2].Err = errors.New("investigate: boom")
	lock := compareRow(t, BuildSupportCompare(off, on), "lock_blocking")
	if lock.Runs != 3 || lock.OffKept != 2 || lock.OnKept != 2 || lock.Downgraded != 0 {
		t.Fatalf("lock row with an unscored pair %+v", lock)
	}
}

func TestBuildSupportCompare_EmptyInputs(t *testing.T) {
	c := BuildSupportCompare(nil, nil)
	if !c.ParityOK() || !c.GateOK() {
		t.Fatalf("empty compare %+v", c)
	}
	pooled := compareRow(t, c, PooledFamily)
	if pooled.Runs != 0 || pooled.KeptPct != "n/a" {
		t.Fatalf("empty pooled row %+v", pooled)
	}
	if md := c.Markdown(); !strings.Contains(md, "require_support_citation") {
		t.Fatalf("empty compare markdown %q", md)
	}
}

func TestSupportCompare_MarkdownAndReplayReport(t *testing.T) {
	off, on := pairedRuns()
	c := BuildSupportCompare(off, on)
	md := c.Markdown()
	for _, want := range []string{"### Compare: require_support_citation",
		"| lock_blocking | 4 | 3 | 2 | 1 | 66.7% | fail |",
		"| wal_retention | 1 | 1 | 1 | 0 | 100.0% | pass |",
		"Deterministic-arm parity: identical"} {
		if !strings.Contains(md, want) {
			t.Errorf("compare markdown lacks %q:\n%s", want, md)
		}
	}
	rep := ReplayReport{Compare: &c}
	if !strings.Contains(rep.Markdown(), "### Compare: require_support_citation") {
		t.Fatal("the replay report does not carry the compare table")
	}
	raw, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(raw), `"compare":{"setting":`+
		`"require_support_citation"`) {
		t.Fatalf("replay report JSON lacks the compare: %s (%v)", raw, err)
	}
	if strings.Contains(ReplayReport{}.Markdown(), "### Compare") {
		t.Fatal("a replay report without a compare rendered one")
	}
	broken := BuildSupportCompare(off, append([]Result(nil), on[1:]...))
	if !strings.Contains(broken.Markdown(), "Deterministic-arm parity: BROKEN") {
		t.Fatalf("a parity break is not shown:\n%s", broken.Markdown())
	}
}

func TestParseCompare(t *testing.T) {
	cases := map[string]string{"": "",
		"require_support_citation":   CompareSupportCitation,
		" require_support_citation ": CompareSupportCitation}
	for in, want := range cases {
		got, err := ParseCompare(in)
		if err != nil || got != want {
			t.Errorf("ParseCompare(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"bogus", "require_support", "all"} {
		if _, err := ParseCompare(bad); err == nil ||
			!strings.Contains(err.Error(), "require_support_citation") {
			t.Errorf("ParseCompare(%q) error %v, want one naming the allowed setting", bad, err)
		}
	}
}

func TestInvestigatorArm_ReplayCarriesTheSupportSetting(t *testing.T) {
	if cfg := (InvestigatorArm{}).replayInvestigator(); cfg == nil ||
		cfg.RequireSupportCitation {
		t.Fatalf("default arm = %+v, want the rule off (the Phase 1 bench)", cfg)
	}
	if cfg := (InvestigatorArm{RequireSupportCitation: true}).replayInvestigator(); cfg ==
		nil || !cfg.RequireSupportCitation {
		t.Fatalf("arm with the rule = %+v, want it on", cfg)
	}
}

func TestModelOutcomeOf(t *testing.T) {
	if got := modelOutcomeOf(sre.Summary{}); got != "" {
		t.Fatalf("no conclusion: %q", got)
	}
	sum := sre.Summary{ModelConclusion: &sre.ModelConclusion{Outcome: sre.ModelContested}}
	if got := modelOutcomeOf(sum); got != sre.ModelContested {
		t.Fatalf("contest: %q", got)
	}
}
