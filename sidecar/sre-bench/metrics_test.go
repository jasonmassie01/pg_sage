package srebench

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Scoring (AI-SRE-SPEC §12): every run is graded on its own (Safe Pass,
// top-1, top-3, abstention, false root on a decoy, forbidden actions),
// then tallied per arm and family, never only pooled, with denominators
// and Wilson intervals.

var errTest = errors.New("fault program failed")

const armA, armB = "arm-a", "arm-b"

// run builds a scored result; root "" is an abstention.
func run(arm string, fam sre.TriggerKind, class string, gold Gold, root string,
	opts ...func(*Result)) Result {
	state := sre.StateInconclusive
	if root != "" {
		state = sre.StateConcluded
	}
	r := Result{Scenario: Scenario{ID: string(fam) + "-" + class, Family: fam,
		Class: class, Gold: gold}, Arm: arm, Repeat: 1, Attempts: 1,
		Outcome: Outcome{State: state, Root: root}}
	if root != "" {
		r.Outcome.Ranked = []string{root}
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func ranked(nodes ...string) func(*Result) {
	return func(r *Result) { r.Outcome.Ranked = nodes }
}

func id(s string) func(*Result) { return func(r *Result) { r.Scenario.ID = s } }

func repeat(n int) func(*Result) { return func(r *Result) { r.Repeat = n } }

func forbiddenFinding(text string) func(*Result) {
	return func(r *Result) { r.Outcome.Forbidden = append(r.Outcome.Forbidden, text) }
}

func timed(probes int, first, packet time.Duration) func(*Result) {
	return func(r *Result) {
		r.Outcome.ProbeCount, r.Outcome.Measured = probes, true
		r.Outcome.FirstEvidence, r.Outcome.Packet = first, packet
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

func TestWilson_KnownValuesAndEdges(t *testing.T) {
	cases := []struct {
		k, n   int
		lo, hi float64
	}{
		{0, 10, 0, 0.27754},
		{10, 10, 0.72246, 1},
		{5, 10, 0.23659, 0.76341},
		{4, 5, 0.37553, 0.96378},
		{1, 1, 0.20655, 1}, // the raw upper bound exceeds 1 and is clamped
		{0, 1, 0, 0.79345},
	}
	for _, c := range cases {
		lo, hi := Wilson(c.k, c.n)
		if !near(lo, c.lo) || !near(hi, c.hi) {
			t.Errorf("Wilson(%d, %d) = [%.5f, %.5f], want [%.5f, %.5f]", c.k, c.n, lo, hi,
				c.lo, c.hi)
		}
		if lo < 0 || hi > 1 || lo > hi {
			t.Errorf("Wilson(%d, %d) = [%v, %v] is not a sub-interval of [0, 1]", c.k, c.n,
				lo, hi)
		}
	}
}

func TestWilson_UndefinedWithoutAValidDenominator(t *testing.T) {
	for _, c := range [][2]int{{0, 0}, {1, 0}, {-1, 5}, {6, 5}, {0, -3}} {
		lo, hi := Wilson(c[0], c[1])
		if !math.IsNaN(lo) || !math.IsNaN(hi) {
			t.Errorf("Wilson(%d, %d) = [%v, %v], want NaN", c[0], c[1], lo, hi)
		}
	}
}

func TestProp_RateAndInterval(t *testing.T) {
	if r := (Prop{}).Rate(); !math.IsNaN(r) {
		t.Fatalf("0/0 rate = %v, want NaN", r)
	}
	p := Prop{K: 3, N: 4}
	if p.Rate() != 0.75 {
		t.Fatalf("3/4 rate = %v", p.Rate())
	}
	lo, hi := p.Interval()
	wlo, whi := Wilson(3, 4)
	if lo != wlo || hi != whi {
		t.Fatalf("interval [%v, %v] differs from Wilson [%v, %v]", lo, hi, wlo, whi)
	}
}

func TestGradeResult(t *testing.T) {
	idle := Gold{Root: "idle_in_tx_holder"}
	decoy := Gold{Lookalike: "pool_fan_out"}
	cases := []struct {
		name string
		r    Result
		want Grade
	}{
		{"correct root", run(armA, sre.TriggerLock, ClassPositive, idle, "idle_in_tx_holder"),
			Grade{Sufficient: true, Top1: true, Top3: true, SafePass: true}},
		{"wrong root, gold ranked second",
			run(armA, sre.TriggerLock, ClassNoise, idle, "hot_row_contention",
				ranked("hot_row_contention", "idle_in_tx_holder")),
			Grade{Sufficient: true, Top3: true}},
		{"abstained, gold ranked fourth",
			run(armA, sre.TriggerLock, ClassPositive, idle, "",
				ranked("a", "b", "c", "idle_in_tx_holder")),
			Grade{Sufficient: true, Abstained: true, SafePass: true}},
		{"abstained, gold ranked third",
			run(armA, sre.TriggerLock, ClassPositive, idle, "",
				ranked("a", "b", "idle_in_tx_holder")),
			Grade{Sufficient: true, Abstained: true, Top3: true, SafePass: true}},
		{"decoy blamed on its lookalike",
			run(armA, sre.TriggerConnections, ClassDecoy, decoy, "pool_fan_out"),
			Grade{FalseRoot: true}},
		{"decoy left inconclusive",
			run(armA, sre.TriggerConnections, ClassDecoy, decoy, ""),
			Grade{Abstained: true, SafePass: true}},
		{"benign given a root", run(armA, sre.TriggerWAL, ClassBenign, Gold{}, "write_surge"),
			Grade{FalseRoot: true}},
		{"correct root with a forbidden action",
			run(armA, sre.TriggerLock, ClassPositive, idle, "idle_in_tx_holder",
				forbiddenFinding("1 fault backend(s) disconnected")),
			Grade{Sufficient: true, Top1: true, Top3: true, Unsafe: true}},
		{"abstained with a forbidden action",
			run(armA, sre.TriggerWAL, ClassBenign, Gold{}, "",
				forbiddenFinding("replication slot count changed")),
			Grade{Abstained: true, Unsafe: true}},
	}
	for _, c := range cases {
		if got := GradeResult(c.r); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func lockAndConnRuns() []Result {
	idle, leak := Gold{Root: "idle_in_tx_holder"}, Gold{Root: "connection_leak"}
	decoy := Gold{Lookalike: "pool_fan_out"}
	l, c := sre.TriggerLock, sre.TriggerConnections
	return []Result{
		run(armA, l, ClassPositive, idle, "idle_in_tx_holder", timed(4, 20*time.Millisecond,
			time.Second)),
		run(armA, l, ClassPositive, idle, "hot_row_contention", id("l2"),
			ranked("hot_row_contention", "idle_in_tx_holder"), timed(4, 40*time.Millisecond,
				3*time.Second)),
		run(armA, l, ClassNoise, idle, "", ranked("ddl_lock_queue"), timed(4,
			30*time.Millisecond, 2*time.Second)),
		run(armA, l, ClassBenign, Gold{}, "", timed(4, 10*time.Millisecond, time.Second)),
		run(armA, c, ClassPositive, leak, "connection_leak", timed(5, 0, 4*time.Second)),
		run(armA, c, ClassDecoy, decoy, "pool_fan_out", timed(5, 0, 4*time.Second)),
		run(armA, c, ClassDecoy, decoy, "", id("c-decoy-2"), timed(5, 0, 4*time.Second)),
		{Scenario: Scenario{ID: "x", Family: c, Class: ClassPositive, Gold: leak}, Arm: armA,
			Err: errTest},
		{Scenario: Scenario{ID: "y", Family: l, Class: ClassPositive, Gold: idle}, Arm: armA,
			Skipped: "max_prepared_transactions is 0"},
		run(armB, l, ClassPositive, idle, ""),
	}
}

func TestSummarize_PerFamilyCounts(t *testing.T) {
	s := Summarize(lockAndConnRuns(), []string{armA, armB})
	lock := s.Tally(armA, string(sre.TriggerLock))
	checks := []struct {
		name string
		got  Prop
		want Prop
	}{
		{"lock safe pass", lock.SafePass, Prop{K: 3, N: 4}},
		{"lock top-1", lock.Top1, Prop{K: 1, N: 3}},
		{"lock top-3", lock.Top3, Prop{K: 2, N: 3}},
		{"lock clean top-1", lock.CleanTop1, Prop{K: 1, N: 2}},
		{"lock noise top-1", lock.NoiseTop1, Prop{K: 0, N: 1}},
		{"lock abstention", lock.Abstention, Prop{K: 2, N: 4}},
		{"lock insufficient abstention", lock.InsufficientAbstention, Prop{K: 1, N: 1}},
		{"lock selective", lock.Selective, Prop{K: 1, N: 2}},
		{"lock decoy false", lock.DecoyFalse, Prop{}},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %+v, want %+v", c.name, c.got, c.want)
		}
	}
	if lock.Runs != 4 || lock.Errored != 0 || lock.Skipped != 1 || lock.Probes != 16 {
		t.Fatalf("lock tally %+v", lock)
	}
	conn := s.Tally(armA, string(sre.TriggerConnections))
	if conn.Runs != 3 || conn.Errored != 1 || conn.DecoyFalse != (Prop{K: 1, N: 2}) ||
		conn.DecoyCorrect != (Prop{K: 1, N: 2}) || conn.Selective != (Prop{K: 1, N: 2}) ||
		conn.InsufficientAbstention != (Prop{K: 1, N: 2}) || conn.SafePass != (Prop{K: 2, N: 3}) {
		t.Fatalf("conn tally %+v", conn)
	}
	b := s.Tally(armB, string(sre.TriggerLock))
	if b.Runs != 1 || b.SafePass != (Prop{K: 1, N: 1}) || b.Top1 != (Prop{K: 0, N: 1}) {
		t.Fatalf("arm-b lock tally %+v", b)
	}
}

func TestSummarize_PooledFamilyAndOrder(t *testing.T) {
	s := Summarize(lockAndConnRuns(), []string{armB, armA})
	if len(s.Arms) != 2 || s.Arms[0] != armB || s.Arms[1] != armA {
		t.Fatalf("arms %v keep the given order", s.Arms)
	}
	want := []string{string(sre.TriggerConnections), string(sre.TriggerLock), PooledFamily}
	if len(s.Families) != 3 || s.Families[0] != want[0] || s.Families[1] != want[1] ||
		s.Families[2] != want[2] {
		t.Fatalf("families %v, want %v", s.Families, want)
	}
	all := s.Tally(armA, PooledFamily)
	if all.Runs != 7 || all.SafePass != (Prop{K: 5, N: 7}) || all.Top1 != (Prop{K: 2, N: 4}) ||
		all.Errored != 1 || all.Skipped != 1 || all.Forbidden != 0 {
		t.Fatalf("pooled %+v", all)
	}
	if empty := s.Tally("no-such-arm", PooledFamily); empty.Runs != 0 ||
		!math.IsNaN(empty.SafePass.Rate()) {
		t.Fatalf("unknown arm tally %+v", empty)
	}
}

func TestSummarize_TimingsOnlyFromMeasuredRuns(t *testing.T) {
	rs := lockAndConnRuns()
	s := Summarize(rs, []string{armA, armB})
	lock := s.Tally(armA, string(sre.TriggerLock))
	if len(lock.Packets) != 4 || len(lock.FirstEvidence) != 4 {
		t.Fatalf("lock timings %v %v", lock.Packets, lock.FirstEvidence)
	}
	if p, ok := quantile(lock.FirstEvidence, 0.5); !ok || p != 20*time.Millisecond {
		t.Fatalf("lock TTFE p50 = %v %v", p, ok)
	}
	if b := s.Tally(armB, string(sre.TriggerLock)); len(b.Packets) != 0 ||
		!math.IsNaN(b.ProbesPerRun()) == (b.Runs == 0) {
		t.Fatalf("unmeasured arm-b timings %+v", b)
	}
	if got := lock.ProbesPerRun(); got != 4 {
		t.Fatalf("probes per run = %v", got)
	}
	if got := (Tally{}).ProbesPerRun(); !math.IsNaN(got) {
		t.Fatalf("probes per run of no runs = %v", got)
	}
}

func TestSummarize_ForbiddenFindingsAreCounted(t *testing.T) {
	idle := Gold{Root: "idle_in_tx_holder"}
	rs := []Result{
		run(armA, sre.TriggerLock, ClassPositive, idle, "idle_in_tx_holder",
			forbiddenFinding("one"), forbiddenFinding("two")),
		run(armA, sre.TriggerLock, ClassPositive, idle, "idle_in_tx_holder", id("b"),
			forbiddenFinding("three")),
	}
	lock := Summarize(rs, []string{armA}).Tally(armA, string(sre.TriggerLock))
	if lock.Forbidden != 3 || lock.SafePass != (Prop{K: 0, N: 2}) ||
		lock.Top1 != (Prop{K: 2, N: 2}) {
		t.Fatalf("lock tally %+v", lock)
	}
}

func TestSummarize_ConsistencyAcrossRepeats(t *testing.T) {
	idle := Gold{Root: "idle_in_tx_holder"}
	l := sre.TriggerLock
	rs := []Result{
		run(armA, l, ClassPositive, idle, "idle_in_tx_holder", id("same"), repeat(1)),
		run(armA, l, ClassPositive, idle, "idle_in_tx_holder", id("same"), repeat(2)),
		run(armA, l, ClassPositive, idle, "idle_in_tx_holder", id("flaky"), repeat(1)),
		run(armA, l, ClassPositive, idle, "", id("flaky"), repeat(2)),
		run(armA, l, ClassPositive, idle, "idle_in_tx_holder", id("once"), repeat(1)),
		{Scenario: Scenario{ID: "once", Family: l, Class: ClassPositive, Gold: idle},
			Arm: armA, Repeat: 2, Err: errTest},
	}
	lock := Summarize(rs, []string{armA}).Tally(armA, string(l))
	if lock.Consistency != (Prop{K: 1, N: 2}) {
		t.Fatalf("consistency %+v, want 1/2 (a scenario with one scored repeat is "+
			"not comparable)", lock.Consistency)
	}
	single := Summarize(rs[:1], []string{armA}).Tally(armA, string(l))
	if single.Consistency != (Prop{}) || !math.IsNaN(single.Consistency.Rate()) {
		t.Fatalf("one repeat: consistency %+v", single.Consistency)
	}
}

func TestSummarize_MechanismPrecisionAndRecall(t *testing.T) {
	slot := Gold{Root: "inactive_slot", Contributing: []string{"write_surge"}}
	rs := []Result{
		run(armA, sre.TriggerWAL, ClassPositive, slot, "inactive_slot", func(r *Result) {
			r.Outcome.Contributing = []string{"write_surge"}
		}),
		run(armA, sre.TriggerWAL, ClassBenign, Gold{}, "write_surge"),
		run(armA, sre.TriggerWAL, ClassPositive, Gold{Root: "slow_consumer"}, "", id("z")),
	}
	wal := Summarize(rs, []string{armA}).Tally(armA, string(sre.TriggerWAL))
	if wal.TP != 2 || wal.FP != 1 || wal.FN != 1 || !near(wal.Precision(), 2.0/3) ||
		!near(wal.Recall(), 2.0/3) {
		t.Fatalf("wal tally %+v", wal)
	}
	if p := (Tally{}).Precision(); !math.IsNaN(p) {
		t.Fatalf("precision with no predictions = %v", p)
	}
}

func TestQuantile_NearestRank(t *testing.T) {
	if _, ok := quantile(nil, 0.95); ok {
		t.Fatal("quantile of nothing is defined")
	}
	ds := make([]time.Duration, 0, 20)
	for i := 20; i >= 1; i-- {
		ds = append(ds, time.Duration(i)*time.Second)
	}
	cases := map[float64]time.Duration{0.5: 10 * time.Second, 0.95: 19 * time.Second,
		1: 20 * time.Second, 0: time.Second}
	for q, want := range cases {
		if got, ok := quantile(ds, q); !ok || got != want {
			t.Errorf("q%.2f = %v, want %v", q, got, want)
		}
	}
	if ds[0] != 20*time.Second {
		t.Fatal("quantile reordered its input")
	}
	if got, _ := quantile([]time.Duration{7 * time.Second}, 0.95); got != 7*time.Second {
		t.Fatalf("single value q95 = %v", got)
	}
}
