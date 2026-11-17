package config

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Boundary: at most one restart-bound candidate per review (the plan-02
// cap). The second goes to the next review, named in the coverage notes.
func TestEmit_OneRestartBoundCandidatePerReview(t *testing.T) {
	p := newFixture().pack(t)
	v, ok, err := readView(p)
	if err != nil || !ok {
		t.Fatalf("read the view: ok=%v err=%v", ok, err)
	}
	cov := candidate.Coverage{Status: candidate.CoverageComplete}
	b := newBuilder(v, &cov)
	b.emit(finding{kind: KindSharedBuffers, setting: "shared_buffers", value: "4GB",
		evidence: []string{"setting/shared_buffers"}, reason: "first"})
	b.emit(finding{kind: "max_connections_peak", setting: "max_connections", value: "200",
		evidence: []string{"setting/max_connections"}, reason: "second"})
	b.emit(finding{kind: KindWorkMem, setting: "work_mem", value: "8MB",
		evidence: []string{"setting/work_mem"}, reason: "reloadable"})
	if len(b.out) != 2 || b.out[0].Target != "shared_buffers" || b.out[1].Target != "work_mem" {
		t.Fatalf("emitted %+v, want shared_buffers then work_mem", b.out)
	}
	if !notesContain(cov, "max_connections", "next review") {
		t.Fatalf("notes %v", cov.Notes)
	}
}

// State transitions: an operator candidate in a plan is a "you run this"
// step (plan-02d). It passes the plan validator as an operator step with
// its landing, the executor classifier never sees it (no SQL, so
// NeedsOwnApproval does not classify it), and the plan refuses it as soon
// as anyone gives it SQL for pg_sage to run or a revert pg_sage would do.
func TestOperatorCandidate_IsAYouRunThisStep(t *testing.T) {
	f := newFixture().with("shared_buffers", "16384", "8kB", "configuration file")
	cands, _ := generate(t, f)
	c := only(t, cands, KindSharedBuffers)
	step := changeplan.Step{Ordinal: 1, Kind: changeplan.KindOperator, Setting: c.Target,
		OperatorCommand: c.Operator.Command, OperatorRevert: c.Operator.Revert,
		Landing:     &changeplan.Landing{Setting: c.Operator.Setting, Value: c.Operator.Value},
		EvidenceIDs: c.EvidenceIDs, Metric: changeplan.MetricReadback,
		ExpectedDirection: changeplan.DirectionNone, ExpectedSource: changeplan.SourceNone,
		DeadlineMinutes: 60}
	plan := changeplan.Plan{Goal: "size shared_buffers to memory", Origin: "generator",
		Scope:  changeplan.Scope{Objects: []string{"shared_buffers"}},
		PackHash: strings.Repeat("a", 64), Status: changeplan.StatusProposed,
		CreatedBy: "review", Steps: []changeplan.Step{step}}
	problems, err := changeplan.Validate(context.Background(), plan, okResolver{})
	if err != nil || len(problems) != 0 {
		t.Fatalf("operator step from the candidate: problems %+v err %v", problems, err)
	}
	if needs, why := changeplan.NeedsOwnApproval(step, nil); needs {
		t.Fatalf("an operator step was classified for the executor: %s", why)
	}
	step.ProposedSQL = "ALTER SYSTEM SET shared_buffers = '4GB'"
	step.OnRegressed = changeplan.ActionRevertStep
	plan.Steps = []changeplan.Step{step}
	problems, _ = changeplan.Validate(context.Background(), plan, okResolver{})
	codes := map[string]bool{}
	for _, p := range problems {
		codes[p.Code] = true
	}
	if !codes[changeplan.CodeSQLNotApplicable] || !codes[changeplan.CodeOperatorNotRevertible] {
		t.Fatalf("SQL and a revert on the operator step: problems %+v", problems)
	}
}

// okResolver says every citation exists.
type okResolver struct{}

func (okResolver) FindingExists(context.Context, int64) (bool, error) { return true, nil }
func (okResolver) EvidenceExists(context.Context, string, string) (bool, error) {
	return true, nil
}
func (okResolver) TargetCalls(context.Context, []int64, time.Duration) (int64, error) {
	return 1000, nil
}
func (okResolver) SettingExists(context.Context, string) (bool, error)     { return true, nil }
func (okResolver) ManagedChangeExists(context.Context, int64) (bool, error) { return true, nil }

// CHECK-15: for every GUC pgconf.AdvisorGUC accepts, Clamp renders any
// computed value, however wild, inside pgconf.ValidateValue's range.
func TestClamp_EveryAdvisorGUCStaysInRange(t *testing.T) {
	wild := []float64{math.Inf(-1), -1e18, -1, 0, 0.0004, 0.5, 1, 1.05, 3.999, 1e3, 7e6,
		1 << 30, 1e12, 1e18, math.Inf(1), math.NaN()}
	for name := range pgconf.Docs {
		if !pgconf.AdvisorGUC(name) {
			t.Fatalf("%s is in Docs but not an advisor GUC", name)
		}
		for _, v := range wild {
			s, ok := Clamp(name, v)
			if !ok {
				t.Fatalf("Clamp(%s, %g) refused a documented GUC", name, v)
			}
			if valid, why := pgconf.ValidateValue(name, s); !valid {
				t.Fatalf("Clamp(%s, %g) = %q, outside the range: %s", name, v, s, why)
			}
		}
	}
	if _, ok := Clamp("not_a_guc", 1); ok {
		t.Fatal("Clamp accepted an undocumented setting")
	}
}

// CHECK-15, end to end: under extreme workloads and hardware on every
// platform kind, every value the generator emits passes ValidateValue.
func TestGenerate_EmittedValuesAlwaysValidate(t *testing.T) {
	for _, platform := range []string{"postgres", "rds", "cloud-sql", "azure", "neon", ""} {
		for _, mem := range []float64{64 << 20, 16 * gib, 1 << 50} {
			f := newFixture().with("shared_buffers", "1", "8kB", "configuration file").
				with("effective_io_concurrency", "0", "", "configuration file").
				with("work_mem", "2097152", "kB", "configuration file")
			f.platform, f.version, f.memoryBytes = platform, "18.0", mem
			f.measures.TempBytesPerHour = 1e18
			f.measures.CheckpointsRequested, f.measures.CheckpointsTimed = 1e9, 0.0
			f.measures.WALBytesPerHour = 1e18
			cands, _ := generate(t, f)
			for _, c := range cands {
				name, value := emittedValue(t, c)
				if name == "" {
					continue
				}
				if ok, why := pgconf.ValidateValue(name, value); !ok {
					t.Fatalf("%s mem %g: %s=%s emitted out of range: %s", platform, mem,
						name, value, why)
				}
			}
		}
	}
}

// emittedValue is the setting and value a candidate would set ("" for
// advice).
func emittedValue(t *testing.T, c candidate.Candidate) (string, string) {
	t.Helper()
	switch c.ActionType {
	case candidate.ActionOperator:
		return c.Operator.Setting, c.Operator.Value
	case "alter_system_guc":
		st, ok := pgconf.ParseAlterSystem(c.ForwardSQL)
		if !ok {
			t.Fatalf("unparseable step SQL %q", c.ForwardSQL)
		}
		return st.Name, st.Value
	}
	return "", ""
}
