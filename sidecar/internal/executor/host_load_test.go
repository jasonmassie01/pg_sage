package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

type testHostLoad struct {
	verify.ObservationSource
	load verify.LoadSample
	err  error
}

func (s testHostLoad) CurrentLoad(ctx context.Context) (verify.LoadSample, error) {
	if ctx.Err() != nil {
		return verify.LoadSample{}, ctx.Err()
	}
	return s.load, s.err
}

func TestHostedLoadSourceTransitions(t *testing.T) {
	e := &Executor{}
	fallback := testHostLoad{load: verify.LoadSample{CPUPct: 22, DataIOPct: 33, LogIOPct: 44}}
	source := executorObservationSource{ObservationSource: fallback, executor: e}
	got, err := source.CurrentLoad(context.Background())
	if err != nil || got != fallback.load {
		t.Fatalf("fallback: %+v %v", got, err)
	}
	host := testHostLoad{load: verify.LoadSample{CPUPct: 50, DataIOPct: 60, LogIOPct: 70}}
	e.WithHostLoadReader(host)
	got, err = source.CurrentLoad(context.Background())
	if err != nil || got != host.load {
		t.Fatalf("provider: %+v %v", got, err)
	}
	unavailable := errors.New("provider I/O is unknown")
	e.WithHostLoadReader(testHostLoad{err: unavailable})
	got, err = source.CurrentLoad(context.Background())
	if !errors.Is(err, unavailable) || got != (verify.LoadSample{}) {
		t.Fatalf("provider error must not use fallback: %+v %v", got, err)
	}
	e.WithHostLoadReader(nil)
	got, err = source.CurrentLoad(context.Background())
	if err != nil || got != fallback.load {
		t.Fatalf("removed provider: %+v %v", got, err)
	}
}

func TestHostedLoadSourceCanceledAndConcurrent(t *testing.T) {
	e := &Executor{}
	reader := testHostLoad{load: verify.LoadSample{CPUPct: 10}}
	source := executorObservationSource{ObservationSource: reader, executor: e}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.CurrentLoad(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				e.WithHostLoadReader(reader)
				got, err := source.CurrentLoad(context.Background())
				if err != nil || got != reader.load {
					t.Errorf("concurrent load: %+v %v", got, err)
				}
				e.WithHostLoadReader(nil)
			}
		}()
	}
	workers.Wait()
}

// No malformed payload tests: the reader accepts only a typed LoadSample.
// Freshness, numeric bounds, and real provider errors are covered by providerobs and verify.
