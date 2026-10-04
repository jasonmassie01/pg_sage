package perfgate

import (
	"strings"
	"testing"
	"time"
)

// Gate G: the sidecar's own process CPU per collector cycle in the
// steady phase (the queued follow-up "budget sidecar CPU per cycle").

func cpuPhase(cpu time.Duration) Phase {
	p := steadyPhase() // 6 cycles
	p.CPUKnown, p.ProcessCPU = true, cpu
	return p
}

func TestCPUGateUnderBudget(t *testing.T) {
	b := DefaultBudgets()
	cpu := time.Duration(b.SidecarCPUMsPerCycle*6) * time.Millisecond // exactly at budget
	got, err := Evaluate([]Phase{cpuPhase(cpu)}, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("at budget: offenders = %+v", got)
	}
}

func TestCPUGateOverBudget(t *testing.T) {
	b := DefaultBudgets()
	cpu := time.Duration(b.SidecarCPUMsPerCycle*6+60) * time.Millisecond // +10 ms/cycle
	got, err := Evaluate([]Phase{cpuPhase(cpu)}, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Gate != GateSidecarCPU {
		t.Fatalf("offenders = %+v, want one %s", got, GateSidecarCPU)
	}
	o := got[0]
	if o.Measured != b.SidecarCPUMsPerCycle+10 || o.Budget != b.SidecarCPUMsPerCycle ||
		o.Unit != "ms per cycle" {
		t.Fatalf("offender = %+v", o)
	}
}

// The warmup phase is not charged per cycle (startup work, keyframes).
func TestCPUGateIgnoresWarmup(t *testing.T) {
	p := cpuPhase(time.Hour)
	p.Name, p.Steady, p.Cycles = "warmup", false, 0
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("warmup charged: %+v", got)
	}
}

// A phase whose CPU was not read is not charged (the harness fails the
// run itself when the reading is impossible).
func TestCPUGateUnknownIsNotCharged(t *testing.T) {
	p := steadyPhase()
	p.ProcessCPU = time.Hour
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown CPU charged: %+v", got)
	}
}

func TestCPUBudgetMustBePositive(t *testing.T) {
	b := DefaultBudgets()
	if b.SidecarCPUMsPerCycle <= 0 {
		t.Fatalf("default sidecar CPU budget = %v", b.SidecarCPUMsPerCycle)
	}
	b.SidecarCPUMsPerCycle = 0
	if _, err := Evaluate([]Phase{steadyPhase()}, b); err == nil ||
		!strings.Contains(err.Error(), "positive") {
		t.Fatalf("zero CPU budget: err = %v", err)
	}
}

func TestReportShowsSidecarCPU(t *testing.T) {
	b := DefaultBudgets()
	md := RenderMarkdown(SmallScale(), b, []Phase{cpuPhase(1200 * time.Millisecond)}, nil)
	for _, want := range []string{string(GateSidecarCPU), "sidecar CPU: 200.0 ms per cycle"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report lacks %q:\n%s", want, md)
		}
	}
}
