package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// HostCPUReader supplies fresh host CPU utilization, such as a provider's
// node metrics. An error means CPU evidence is unavailable, never 0%.
type HostCPUReader interface {
	CurrentCPU(context.Context) (float64, error)
}

// HostGuardReader is a host CPU reader that also reports reasons to wait
// (managed-cloud replica lag, storage runway, memory pressure). Its
// reasons only withhold admission; they never admit.
type HostGuardReader interface {
	HostWithhold(context.Context) []string
}

// IOEvidenceReader supplies pg-side IO rates and the learned baseline.
type IOEvidenceReader interface {
	IOEvidence(context.Context) (verify.IOEvidence, error)
}

// WithHostCPUReader installs the host CPU source for load admission.
func (e *Executor) WithHostCPUReader(reader HostCPUReader) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.hostCPU = reader
}

// WithIOEvidence installs the pg-side IO sampler for load admission.
func (e *Executor) WithIOEvidence(reader IOEvidenceReader) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.ioEvidence = reader
}

type executorObservationSource struct {
	verify.ObservationSource
	executor *Executor
}

// LoadEvidence composes host CPU, pg-side IO, declared capacity and the
// maintenance window into admission evidence. A CPU read failure is
// missing CPU evidence; an IO sampler failure fails admission closed.
func (s executorObservationSource) LoadEvidence(
	ctx context.Context,
) (verify.LoadEvidence, error) {
	if err := ctx.Err(); err != nil {
		return verify.LoadEvidence{}, err
	}
	e := s.executor
	e.policyMu.RLock()
	cpuReader, ioReader := e.hostCPU, e.ioEvidence
	e.policyMu.RUnlock()
	cfg, _, _ := e.policySnapshot()
	evidence := verify.LoadEvidence{
		Capacity: declaredCapacity(cfg), WindowOpen: e.admissionWindowOpen(ctx),
	}
	if cpuReader != nil {
		if cpu, err := cpuReader.CurrentCPU(ctx); err == nil {
			evidence.CPUPct = &cpu
		}
		if guard, ok := cpuReader.(HostGuardReader); ok {
			evidence.HostWithhold = strings.Join(guard.HostWithhold(ctx), "; ")
		}
	}
	if ioReader == nil {
		evidence.RateError = "pg-side IO sampler is not running"
		return evidence, ctx.Err()
	}
	io, err := ioReader.IOEvidence(ctx)
	if err != nil {
		return verify.LoadEvidence{}, fmt.Errorf("read pg-side IO evidence: %w", err)
	}
	evidence.Rate, evidence.RateError, evidence.Baseline = io.Rate, io.RateError, io.Baseline
	return evidence, ctx.Err()
}

func declaredCapacity(cfg *config.Config) *verify.IOCapacity {
	if cfg == nil || cfg.Verify.IOCapacity == nil {
		return nil
	}
	return &verify.IOCapacity{
		ReadWriteMBps: cfg.Verify.IOCapacity.ReadWriteMBps,
		WALMBps:       cfg.Verify.IOCapacity.WALMBps,
	}
}
