package tuning

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Validation of the configuration, statistics and hint proposals, of
// citations and of predictions.

func gucProposal(name, value string, expected float64) Proposal {
	return Proposal{Type: ProposeGUC, Name: name, Value: value, Evidence: []string{"S1"},
		Rationale: "spills", ExpectedChangePct: pct(expected)}
}

func TestJudge_GUCWorkMem(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	if f.Category != "memory_tuning" || f.ObjectIdentifier != "instance:work_mem" ||
		f.RecommendedSQL != "ALTER SYSTEM SET work_mem = '64MB'" {
		t.Fatalf("finding = %+v", f)
	}
	if f.RollbackSQL != "ALTER SYSTEM RESET work_mem" {
		t.Fatalf("rollback restores the default: %q", f.RollbackSQL)
	}
	p := j.Prediction
	if p.Metric != "temp_spills" || *p.ExpectedChangePct != -60 || p.Method != verify.MethodModel ||
		j.Class != verify.ClassGUC {
		t.Fatalf("prediction = %+v class %q", p, j.Class)
	}
}

func TestJudge_GUCRefusals(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name, guc, value string
		expected         float64
		want             Reason
	}{
		{"out of range", "work_mem", "64TB", -60, ReasonInvalid},
		{"not allowlisted", "fsync", "off", -10, ReasonInvalid},
		{"left to the config advisor", "max_wal_size", "4GB", -10, ReasonOutOfScope},
		{"predicts a regression", "work_mem", "64MB", 30, ReasonInvalid},
		{"impossible fall", "work_mem", "64MB", -150, ReasonInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := judgeOne(t, h, nil, gucProposal(tc.guc, tc.value, tc.expected))
			if j.Verdict != VerdictRejected || j.Reason != tc.want || j.Detail == "" {
				t.Fatalf("judged = %+v", j)
			}
		})
	}
}

func TestJudge_GUCRestartRequiredNeedsApprovalAndGroundedMemory(t *testing.T) {
	s := defaultSettings()
	s.HostMemoryBytes = 16 << 30
	h := newHarnessWith(t, s)
	j := judgeOne(t, h, nil, gucProposal("shared_buffers", "4GB", -20))
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	if _, ok := j.Finding.Detail[analyzer.DetailApprovalRequired]; !ok {
		t.Fatalf("a restart-required change needs an operator: %v", j.Finding.Detail)
	}
	j = judgeOne(t, h, nil, gucProposal("shared_buffers", "8GB", -20))
	if j.Verdict != VerdictRejected || j.Reason != ReasonUnavailable {
		t.Fatalf("50%% of host memory is over the 40%% guard: %+v", j)
	}
	h = newHarness(t) // host memory unknown
	if j := judgeOne(t, h, nil, gucProposal("shared_buffers", "1GB", -20)); j.Reason !=
		ReasonUnavailable {
		t.Fatalf("shared_buffers without host memory is not executable: %+v", j)
	}
}

func TestJudge_GUCOnAManagedService(t *testing.T) {
	s := defaultSettings()
	s.CloudEnv, s.DatabaseName = "rds", "app"
	h := newHarnessWith(t, s)
	j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictAdmitted || !strings.HasPrefix(j.Finding.RecommendedSQL,
		"ALTER DATABASE") || !strings.Contains(j.Finding.RecommendedSQL, "work_mem") {
		t.Fatalf("managed services take ALTER DATABASE: %+v", j)
	}
	if j := judgeOne(t, h, nil, gucProposal("shared_buffers", "1GB", -20)); j.Reason !=
		ReasonUnavailable {
		t.Fatalf("a restart GUC on RDS is console-only: %+v", j)
	}
}

func relProposal(option, value string, expected float64) Proposal {
	return Proposal{Type: ProposeReloption, Table: "public.orders", Option: option,
		Value: value, Evidence: []string{"T1"}, Rationale: "dead tuples",
		ExpectedChangePct: pct(expected)}
}

func TestJudge_Reloption(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, relProposal("autovacuum_vacuum_scale_factor", "0.02", -40))
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	if f.Category != "vacuum_tuning" ||
		f.ObjectIdentifier != "public.orders:autovacuum_vacuum_scale_factor" ||
		f.RecommendedSQL != "ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)" ||
		f.RollbackSQL != "ALTER TABLE public.orders RESET (autovacuum_vacuum_scale_factor)" {
		t.Fatalf("finding = %+v", f)
	}
	if j.Prediction.Metric != "dead_tuples" || j.Class != verify.ClassReloption {
		t.Fatalf("prediction = %+v", j.Prediction)
	}
	j = judgeOne(t, h, nil, relProposal("fillfactor", "90", 10))
	if j.Verdict != VerdictAdmitted || j.Finding.Category != "table_tuning" ||
		j.Prediction.Metric != "hot_updates" {
		t.Fatalf("fillfactor raises the HOT share: %+v", j)
	}
}

func TestJudge_ReloptionRefusals(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		p    Proposal
		want Reason
	}{
		{"autovacuum off", relProposal("autovacuum_enabled", "false", -10), ReasonInvalid},
		{"out of range", relProposal("autovacuum_vacuum_scale_factor", "5", -10), ReasonInvalid},
		{"unknown option", relProposal("parallel_workers", "8", -10), ReasonInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if j := judgeOne(t, h, nil, tc.p); j.Verdict != VerdictRejected ||
				j.Reason != tc.want {
				t.Fatalf("judged = %+v", j)
			}
		})
	}
	other := relProposal("fillfactor", "90", 10)
	other.Table = "app.invoices"
	if j := judgeOne(t, h, nil, other); j.Reason != ReasonOutOfCase {
		t.Fatalf("another table: %+v", j)
	}
}

func statsProposal(cols, kinds []string) Proposal {
	return Proposal{Type: ProposeStatistics, Table: "public.orders", Columns: cols,
		Kinds: kinds, Evidence: []string{"R1"}, Rationale: "correlated filters",
		ExpectedChangePct: pct(-60)}
}

func TestJudge_CreateStatisticsInTheAcceptedForm(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, statsProposal([]string{"customer_id", "status"},
		[]string{"dependencies"}))
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	f := *j.Finding
	parsed, err := extstats.ParseCreate(f.RecommendedSQL)
	if err != nil {
		t.Fatalf("the agent must generate the accepted form: %v (%q)", err, f.RecommendedSQL)
	}
	if parsed.Schema.Name != "public" || parsed.Table.Name != "orders" ||
		!strings.HasPrefix(parsed.Name.Name, extstats.NamePrefix) ||
		len(parsed.Name.Name) > 63 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if f.RollbackSQL != parsed.Rollback() || f.Category != CategoryStatistics {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.HasPrefix(f.ObjectIdentifier, "public.orders:"+extstats.NamePrefix) {
		t.Fatalf("identity = %q", f.ObjectIdentifier)
	}
	if j.Prediction.Metric != verify.MetricRowEstimateError || j.Class != verify.ClassStatistics {
		t.Fatalf("prediction = %+v", j.Prediction)
	}
	again := judgeOne(t, h, nil, statsProposal([]string{"status", "customer_id"},
		[]string{"dependencies"}))
	if again.Finding.RecommendedSQL == "" || again.Finding.ObjectIdentifier != f.ObjectIdentifier {
		t.Fatalf("the name depends on the column set, not its order: %q vs %q",
			again.Finding.ObjectIdentifier, f.ObjectIdentifier)
	}
}

func TestJudge_CreateStatisticsUnsupportedForms(t *testing.T) {
	h := newHarness(t)
	nine := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for _, tc := range []struct {
		name  string
		cols  []string
		kinds []string
		want  Reason
	}{
		{"one column", []string{"status"}, nil, ReasonUnsupported},
		{"nine columns", nine, nil, ReasonUnsupported},
		{"expression", []string{"lower(status)", "customer_id"}, nil, ReasonUnsupported},
		{"unknown kind", []string{"status", "customer_id"}, []string{"histogram"},
			ReasonUnsupported},
		{"repeated column", []string{"status", "status"}, nil, ReasonUnsupported},
		{"unknown column", []string{"status", "nope"}, nil, ReasonInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := judgeOne(t, h, nil, statsProposal(tc.cols, tc.kinds))
			if j.Verdict != VerdictRejected || j.Reason != tc.want {
				t.Fatalf("judged = %+v", j)
			}
		})
	}
}

func TestJudge_CreateStatisticsDuplicateOfExisting(t *testing.T) {
	h := newHarness(t)
	h.store.extStats = []ExtStat{{Name: "orders_stx", Columns: []string{"status",
		"customer_id"}, Kinds: []string{"dependencies"}}}
	j := judgeOne(t, h, nil, statsProposal([]string{"customer_id", "status"}, nil))
	if j.Verdict != VerdictRejected || j.Reason != ReasonDuplicate {
		t.Fatalf("judged = %+v", j)
	}
}

func hintProposal(qid int64) Proposal {
	return Proposal{Type: ProposeQueryHint, QueryID: QueryID(qid),
		Hint: "IndexScan(orders orders_customer_idx)", Evidence: []string{"R1"},
		Rationale: "planner misestimates", ExpectedChangePct: pct(-25)}
}

func TestJudge_QueryHintThroughTheTuner(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, hintProposal(101))
	if j.Verdict != VerdictAdmitted || j.Finding.Category != "query_tuning" {
		t.Fatalf("judged = %+v", j)
	}
	if len(h.hints.got) != 1 || h.hints.got[0].QueryID != 101 ||
		h.hints.got[0].Hint != "IndexScan(orders orders_customer_idx)" ||
		!strings.Contains(h.hints.got[0].Query, "customer_id") {
		t.Fatalf("hint sink got %+v", h.hints.got)
	}
	if j.Finding.Detail["producer"] != Producer || j.Class != verify.ClassQueryHint {
		t.Fatalf("detail = %v class %q", j.Finding.Detail, j.Class)
	}
}

func TestJudge_QueryHintRefusals(t *testing.T) {
	h := newHarness(t)
	if j := judgeOne(t, h, nil, hintProposal(999)); j.Reason != ReasonOutOfCase {
		t.Fatalf("a statement outside the case: %+v", j)
	}
	h.hints.available = false
	if j := judgeOne(t, h, nil, hintProposal(101)); j.Reason != ReasonDisabled {
		t.Fatalf("no pg_hint_plan: %+v", j)
	}
	h.hints.available, h.hints.err = true, errFake
	if j := judgeOne(t, h, nil, hintProposal(101)); j.Reason != ReasonInvalid ||
		!strings.Contains(j.Detail, "fake failure") {
		t.Fatalf("the tuner refused the hint: %+v", j)
	}
	s := defaultSettings()
	s.Allowed[ProposeQueryHint] = false
	h = newHarnessWith(t, s)
	if j := judgeOne(t, h, nil, hintProposal(101)); j.Reason != ReasonDisabled ||
		len(h.hints.got) != 0 {
		t.Fatalf("hints switched off: %+v", j)
	}
}

func TestJudge_CitationsAndForms(t *testing.T) {
	h := newHarness(t)
	uncited := gucProposal("work_mem", "64MB", -60)
	uncited.Evidence = nil
	if j := judgeOne(t, h, nil, uncited); j.Reason != ReasonUncited {
		t.Fatalf("uncited: %+v", j)
	}
	unknown := gucProposal("work_mem", "64MB", -60)
	unknown.Evidence = []string{"S1", "R77"}
	if j := judgeOne(t, h, nil, unknown); j.Reason != ReasonUnknownEvidence ||
		!strings.Contains(j.Detail, "R77") {
		t.Fatalf("unknown citation: %+v", j)
	}
	if j := judgeOne(t, h, nil, Proposal{Type: "vacuum_full", Evidence: []string{"S1"},
		ExpectedChangePct: pct(-10)}); j.Reason != ReasonUnsupported {
		t.Fatalf("unknown type: %+v", j)
	}
	noPred := gucProposal("work_mem", "64MB", 0)
	noPred.ExpectedChangePct = nil
	if j := judgeOne(t, h, nil, noPred); j.Reason != ReasonNoPrediction {
		t.Fatalf("no prediction: %+v", j)
	}
}

func TestJudge_TargetsAreWorkloadStatementsOnly(t *testing.T) {
	h := newHarness(t)
	p := gucProposal("work_mem", "64MB", -60)
	p.TargetQueryIDs = []QueryID{101, 999999}
	j := judgeOne(t, h, nil, p)
	if got := j.Prediction.TargetQueryIDs; len(got) != 1 || got[0] != 101 {
		t.Fatalf("targets = %v, want [101] (an unknown statement is dropped)", got)
	}
	p.TargetQueryIDs = nil
	j = judgeOne(t, h, nil, p)
	if got := j.Prediction.TargetQueryIDs; len(got) != 1 || got[0] != 101 {
		t.Fatalf("without targets, the case statements: %v", got)
	}
}

func TestJudge_DisabledTypes(t *testing.T) {
	s := defaultSettings()
	s.Allowed = map[ProposalType]bool{ProposeIndexCreate: true}
	h := newHarnessWith(t, s)
	for _, p := range []Proposal{gucProposal("work_mem", "64MB", -60),
		relProposal("fillfactor", "90", 10), dropProposal("public.orders_customer_idx"),
		statsProposal([]string{"customer_id", "status"}, nil)} {
		if j := judgeOne(t, h, nil, p); j.Reason != ReasonDisabled {
			t.Fatalf("%s switched off: %+v", p.Type, j)
		}
	}
}
