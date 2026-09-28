package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

type testHostCPU struct {
	cpu float64
	err error
}

func (s testHostCPU) CurrentCPU(ctx context.Context) (float64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return s.cpu, s.err
}

func evidenceSource(e *Executor) executorObservationSource {
	return executorObservationSource{
		ObservationSource: verify.NewPostgresObservationSource(nil), executor: e,
	}
}

func TestHostCPUReaderTransitions(t *testing.T) {
	e := &Executor{cfg: config.DefaultConfig()}
	e.WithIOEvidence(fakeIOEvidence{evidence: quietIOEvidence()})
	source := evidenceSource(e)
	got, err := source.LoadEvidence(context.Background())
	if err != nil || got.CPUPct != nil {
		t.Fatalf("no reader: CPU %v err %v, want unavailable CPU", got.CPUPct, err)
	}
	e.WithHostCPUReader(testHostCPU{cpu: 50})
	got, err = source.LoadEvidence(context.Background())
	if err != nil || got.CPUPct == nil || *got.CPUPct != 50 {
		t.Fatalf("provider CPU: %+v %v", got.CPUPct, err)
	}
	// A provider error is missing CPU evidence, never a quiet 0% host.
	e.WithHostCPUReader(testHostCPU{err: errors.New("metrics endpoint down")})
	got, err = source.LoadEvidence(context.Background())
	if err != nil || got.CPUPct != nil {
		t.Fatalf("provider error: CPU %v err %v", got.CPUPct, err)
	}
	e.WithHostCPUReader(nil)
	got, err = source.LoadEvidence(context.Background())
	if err != nil || got.CPUPct != nil || got.Rate == nil {
		t.Fatalf("removed provider: %+v %v", got, err)
	}
}

func TestHostCPUReaderCanceledAndConcurrent(t *testing.T) {
	e := &Executor{cfg: config.DefaultConfig()}
	e.WithIOEvidence(fakeIOEvidence{evidence: quietIOEvidence()})
	reader := testHostCPU{cpu: 10}
	source := evidenceSource(e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.LoadEvidence(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				e.WithHostCPUReader(reader)
				got, err := source.LoadEvidence(context.Background())
				if err != nil || got.Rate == nil {
					t.Errorf("concurrent load: %+v %v", got, err)
				}
				e.WithHostCPUReader(nil)
			}
		}()
	}
	workers.Wait()
}

// No malformed payload tests: the reader accepts only a typed float.
// Freshness and numeric bounds are covered by providerobs and verify.
