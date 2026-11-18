package specialist

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// ConformanceFixture exposes this package's test doubles to the external
// conformance harness (conformance_test.go), which drives the contract
// through the MCP server and so must live in package specialist_test.
type ConformanceFixture struct{ h *harness }

// NewConformanceFixture is a service over the fake "orders" backend.
func NewConformanceFixture(t *testing.T) *ConformanceFixture {
	t.Helper()
	return &ConformanceFixture{h: newHarness(t, DefaultLimits())}
}

// Service is the contract under test.
func (f *ConformanceFixture) Service() *Service { return f.h.svc }

// Run moves investigation id to collecting evidence, as the coordinator
// does once it picks it up.
func (f *ConformanceFixture) Run(id string) {
	f.h.orders.setState(sre.UUID(id), sre.StateCollecting)
}

// Conclude finishes investigation id with the lock diagnosis and one
// requestable cancel proposal.
func (f *ConformanceFixture) Conclude(id string) {
	b := f.h.orders
	d := lockDetail()
	d.Investigation.ID = sre.UUID(id)
	p := cancelProposal(sreaction.ProposalProposed)
	p.InvestigationID = sre.UUID(id)
	b.put(d)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.proposals[sre.UUID(id)] = []sreaction.ProposalView{p}
}

// Requested lists "proposal-id|actor" for every remediation requested.
func (f *ConformanceFixture) Requested() []string {
	b := f.h.orders
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.requested...)
}

// ConformanceQueueID is the approval-queue id the fake gives a requested proposal.
const ConformanceQueueID = 91
