package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/verify"
)

// HostLoadReader supplies fresh, complete provider load evidence to index admission.
type HostLoadReader interface {
	CurrentLoad(context.Context) (verify.LoadSample, error)
}

// WithHostLoadReader replaces only load collection, preserving in-flight verification watches.
// Provider failures propagate; unavailable evidence never falls back to a fabricated quiet host.
func (e *Executor) WithHostLoadReader(reader HostLoadReader) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.hostLoad = reader
}

type executorObservationSource struct {
	verify.ObservationSource
	executor *Executor
}

func (s executorObservationSource) CurrentLoad(ctx context.Context) (verify.LoadSample, error) {
	if err := ctx.Err(); err != nil {
		return verify.LoadSample{}, err
	}
	s.executor.policyMu.RLock()
	reader := s.executor.hostLoad
	s.executor.policyMu.RUnlock()
	if reader != nil {
		return reader.CurrentLoad(ctx)
	}
	return s.ObservationSource.CurrentLoad(ctx)
}
