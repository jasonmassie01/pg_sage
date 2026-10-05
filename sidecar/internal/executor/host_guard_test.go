package executor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// A host CPU reader that also guards the host (managed-cloud telemetry:
// replica lag, storage runway, memory pressure) contributes withhold
// reasons to load admission; a plain CPU reader contributes none.

type guardingHostCPU struct {
	testHostCPU
	reasons []string
}

func (g guardingHostCPU) HostWithhold(context.Context) []string { return g.reasons }

func TestHostGuardReaderContributesWithholdReasons(t *testing.T) {
	e := &Executor{cfg: config.DefaultConfig()}
	e.WithIOEvidence(fakeIOEvidence{evidence: quietIOEvidence()})
	source := evidenceSource(e)
	e.WithHostCPUReader(guardingHostCPU{testHostCPU: testHostCPU{cpu: 30},
		reasons: []string{"replica lag 45s exceeds 30s"}})
	got, err := source.LoadEvidence(context.Background())
	if err != nil || got.CPUPct == nil || *got.CPUPct != 30 ||
		len(got.HostWithhold) != 1 || got.HostWithhold[0] != "replica lag 45s exceeds 30s" {
		t.Fatalf("guarding reader evidence = %+v, %v", got, err)
	}
	e.WithHostCPUReader(testHostCPU{cpu: 30})
	got, err = source.LoadEvidence(context.Background())
	if err != nil || len(got.HostWithhold) != 0 {
		t.Fatalf("plain reader must add no reasons: %+v, %v", got.HostWithhold, err)
	}
}

// The guard is read even when CPU is unavailable: a withhold reason does
// not depend on CPU evidence.
func TestHostGuardReaderWithoutCPU(t *testing.T) {
	e := &Executor{cfg: config.DefaultConfig()}
	e.WithIOEvidence(fakeIOEvidence{evidence: quietIOEvidence()})
	e.WithHostCPUReader(guardingHostCPU{testHostCPU: testHostCPU{err: context.DeadlineExceeded},
		reasons: []string{"free storage 3.0% is below 10%"}})
	got, err := evidenceSource(e).LoadEvidence(context.Background())
	if err != nil || got.CPUPct != nil || len(got.HostWithhold) != 1 {
		t.Fatalf("evidence = %+v, %v", got, err)
	}
}
