package autonomy

import (
	"context"
	"sync"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

type recordingCustodian struct {
	mu        sync.Mutex
	calls     int
	proposals []Proposal
	err       error
	started   chan struct{}
	block     bool
}

func (c *recordingCustodian) Scan(ctx context.Context) ([]Proposal, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.started != nil {
		select {
		case c.started <- struct{}{}:
		default:
		}
	}
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]Proposal(nil), c.proposals...), c.err
}

func (c *recordingCustodian) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type recordingRouter struct {
	mu        sync.Mutex
	proposals []Proposal
}

func (r *recordingRouter) Route(_ context.Context, proposal Proposal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proposals = append(r.proposals, proposal)
	return nil
}

func (r *recordingRouter) RouteVerifiedIndex(
	ctx context.Context, proposal Proposal, _ string, _ []int64,
) error {
	return r.Route(ctx, proposal)
}

func (r *recordingRouter) routed() []Proposal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Proposal(nil), r.proposals...)
}

type recordingAuditor struct {
	mu     sync.Mutex
	calls  int
	result ledger.AuditResult
	err    error
}

type recordingSchemaGuard struct {
	mu      sync.Mutex
	calls   int
	result  schemaguard.CycleResult
	err     error
	started chan struct{}
	block   bool
}

func (g *recordingSchemaGuard) Scan(ctx context.Context) (schemaguard.CycleResult, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	if g.started != nil {
		select {
		case g.started <- struct{}{}:
		default:
		}
	}
	if g.block {
		<-ctx.Done()
		return schemaguard.CycleResult{}, ctx.Err()
	}
	return g.result, g.err
}

func (g *recordingSchemaGuard) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (a *recordingAuditor) SelfAudit(context.Context) (ledger.AuditResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.result, a.err
}

func (a *recordingAuditor) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type report struct {
	level, message string
	fields         map[string]any
}

type recordingReporter struct {
	mu      sync.Mutex
	reports []report
}

func (r *recordingReporter) Report(level, message string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, report{level, message, fields})
}

func (r *recordingReporter) snapshot() []report {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]report(nil), r.reports...)
}
