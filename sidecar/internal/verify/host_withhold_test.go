package verify

import (
	"strings"
	"testing"
	"time"
)

// Host telemetry (managed clouds) can only withhold an admission the IO
// and CPU rules would grant: replica lag, storage runway or memory
// pressure. It never admits anything by itself.

func admittedEvidence() LoadEvidence {
	cpu := 20.0
	return LoadEvidence{CPUPct: &cpu, WindowOpen: false,
		Rate:     &IORate{DataBytesPerSec: 1 << 20, WALBytesPerSec: 1 << 18, Interval: time.Minute},
		Capacity: &IOCapacity{ReadWriteMBps: 100, WALMBps: 100}}
}

func TestDecideAdmissionHostWithhold(t *testing.T) {
	opts := DefaultOptions()
	base := DecideAdmission(admittedEvidence(), opts)
	if !base.OK {
		t.Fatalf("fixture must be admitted without telemetry: %+v", base)
	}
	ev := admittedEvidence()
	ev.HostWithhold = "replica lag 45s exceeds 30s"
	got := DecideAdmission(ev, opts)
	if got.OK || got.Reason != ReasonHostTelemetry ||
		!strings.Contains(got.Detail, "replica lag 45s") {
		t.Fatalf("withheld admission = %+v", got)
	}
	if got.Evidence["host_withhold"] == nil {
		t.Fatalf("the reasons are recorded as evidence: %v", got.Evidence)
	}
}

func TestDecideAdmissionHostWithholdKeepsEarlierRefusal(t *testing.T) {
	ev := admittedEvidence()
	high := 95.0
	ev.CPUPct = &high
	ev.HostWithhold = "free storage 3.0% is below 10%"
	got := DecideAdmission(ev, DefaultOptions())
	if got.OK || got.Reason != ReasonCPUCeiling {
		t.Fatalf("the first refusal stands: %+v", got)
	}
}

func TestDecideAdmissionEmptyHostWithholdIsNeutral(t *testing.T) {
	ev := admittedEvidence()
	ev.HostWithhold = ""
	if got := DecideAdmission(ev, DefaultOptions()); !got.OK {
		t.Fatalf("no reasons must not withhold: %+v", got)
	}
	ev.HostWithhold = "  "
	if got := DecideAdmission(ev, DefaultOptions()); !got.OK {
		t.Fatalf("blank reasons must not withhold: %+v", got)
	}
}
