package specialist

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Test doubles for the per-database backend and the request store. The
// real ones are exercised against PostgreSQL in the *_db_test.go files.

type fakeBackend struct {
	mu        sync.Mutex
	invs      map[sre.UUID]sre.Detail
	proposals map[sre.UUID][]sreaction.ProposalView
	started   []sre.Trigger
	requested []string // "proposal-id|actor"
	submitted []string // "feature|actor"
	outcome   GateOutcome
	reqErr    error
	startErr  error
	detailErr error
	propErr   error
	listErr   error
	listCalls int
	// Query-hash resolution and transcripts (contract revision 1.1.0).
	hashes         map[string][]int64
	hashErr        error
	hashCalls      []string
	transcripts    map[sre.UUID]sre.TranscriptView
	transcriptErr  error
	transcriptKeep []bool
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{invs: map[sre.UUID]sre.Detail{},
		proposals: map[sre.UUID][]sreaction.ProposalView{}}
}

func (b *fakeBackend) put(d sre.Detail) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.invs[d.Investigation.ID] = d
}

func (b *fakeBackend) setState(id sre.UUID, st sre.State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.invs[id]
	d.Investigation.State = st
	b.invs[id] = d
}

func (b *fakeBackend) Start(_ context.Context, t sre.Trigger) (sre.Investigation, bool,
	error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return sre.Investigation{}, false, b.startErr
	}
	b.started = append(b.started, t)
	for _, d := range b.invs {
		inv := d.Investigation
		if inv.State.Live() && inv.TriggerKind == t.Kind && inv.Subject == t.Subject {
			return inv, false, nil // the store coalesces a live trigger
		}
	}
	inv := sre.Investigation{ID: sre.NewUUID(), CaseID: t.CaseID, TriggerKind: t.Kind,
		Subject: t.Subject, State: sre.StateQueued, CreatedAt: created, UpdatedAt: created}
	b.invs[inv.ID] = sre.Detail{Database: "orders", Investigation: inv}
	return inv, true, nil
}

func (b *fakeBackend) Detail(_ context.Context, id sre.UUID) (sre.Detail, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.detailErr != nil {
		return sre.Detail{}, b.detailErr
	}
	d, ok := b.invs[id]
	if !ok {
		return sre.Detail{}, fmt.Errorf("%w: %s", sre.ErrNotFound, id)
	}
	return d, nil
}

func (b *fakeBackend) List(_ context.Context, f sre.ListFilter) (sre.Page, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls++
	if b.listErr != nil {
		return sre.Page{}, b.listErr
	}
	items := []sre.Investigation{}
	for _, d := range b.invs {
		items = append(items, d.Investigation)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return sre.Page{Items: items}, nil
}

func (b *fakeBackend) Proposals(_ context.Context, id sre.UUID) ([]sreaction.ProposalView,
	error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.propErr != nil {
		return nil, b.propErr
	}
	return append([]sreaction.ProposalView(nil), b.proposals[id]...), nil
}

func (b *fakeBackend) RequestProposal(_ context.Context, id sre.UUID,
	actor string) (sreaction.ProposalView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requested = append(b.requested, string(id)+"|"+actor)
	for _, list := range b.proposals {
		for _, p := range list {
			if p.ID == id {
				if b.reqErr != nil {
					return p, b.reqErr
				}
				p.State, p.QueueID, p.RequestedBy = sreaction.ProposalRequested, 91, actor
				return p, nil
			}
		}
	}
	return sreaction.ProposalView{}, sreaction.ErrProposalNotFound
}

func (b *fakeBackend) SubmitCustodian(_ context.Context, _ sre.Investigation,
	p sre.ActionProposal, actor string) (GateOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.submitted = append(b.submitted, p.Feature+"|"+actor)
	if b.reqErr != nil {
		return GateOutcome{}, b.reqErr
	}
	return b.outcome, nil
}

type fakeDirectory map[string]Backend

func (d fakeDirectory) Backend(name string) (Backend, bool) {
	b, ok := d[name]
	return b, ok
}

type memStore struct {
	mu       sync.Mutex
	records  []Record
	err      error
	terminal map[string]bool
}

func newMemStore() *memStore { return &memStore{terminal: map[string]bool{}} }

func (m *memStore) Record(_ context.Context, r Record) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return Record{}, m.err
	}
	r.ID = fmt.Sprintf("rec-%d", len(m.records)+1)
	if r.CreatedAt.IsZero() {
		r.CreatedAt = created
	}
	m.records = append(m.records, r)
	return r, nil
}

func (m *memStore) LiveOpened(_ context.Context, tokenID string) ([]LiveRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	var out []LiveRef
	for _, r := range m.records {
		if r.Kind != KindOpen || !r.Created || m.terminal[r.ID] ||
			(tokenID != "" && r.TokenID != tokenID) {
			continue
		}
		out = append(out, LiveRef{RecordID: r.ID, Database: r.Database,
			InvestigationID: r.InvestigationID})
	}
	return out, nil
}

func (m *memStore) MarkTerminal(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.terminal[id] = true
	return nil
}

func (m *memStore) ForInvestigation(_ context.Context, tokenID, database,
	invID string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	for i := len(m.records) - 1; i >= 0; i-- {
		r := m.records[i]
		if r.TokenID == tokenID && r.Database == database && r.InvestigationID == invID &&
			(r.Kind == KindOpen || r.Kind == KindAttach) {
			return &r, nil
		}
	}
	return nil, nil
}

func (m *memStore) HasExternal(_ context.Context, system, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	for _, r := range m.records {
		if r.ExternalRef != nil && r.ExternalRef.System == system && r.ExternalRef.ID == id &&
			r.Outbound != OutboundNone {
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) Recent(_ context.Context, limit int) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Record(nil), m.records...)
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, m.err
}

func (m *memStore) ClaimOutbound(_ context.Context, now time.Time, lease time.Duration,
	limit int) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	var out []Record
	for i := range m.records {
		r := &m.records[i]
		if r.Outbound == OutboundPending && !r.OutboundNextAt.After(now) && len(out) < limit {
			r.OutboundNextAt = now.Add(lease)
			out = append(out, *r)
		}
	}
	return out, nil
}

func (m *memStore) FinishOutbound(_ context.Context, id, state, errText string,
	next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.records {
		if m.records[i].ID == id {
			r := &m.records[i]
			r.Outbound, r.OutboundError, r.OutboundNextAt = state, errText, next
			r.OutboundAttempts++
			return nil
		}
	}
	return errors.New("no such record")
}

func (m *memStore) RescheduleOutbound(_ context.Context, id string, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.records {
		if m.records[i].ID == id {
			m.records[i].OutboundNextAt = next
			return nil
		}
	}
	return errors.New("no such record")
}

func (m *memStore) all() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.records...)
}

func (b *fakeBackend) ResolveQueryHash(_ context.Context, hash string) ([]int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hashCalls = append(b.hashCalls, hash)
	if b.hashErr != nil {
		return nil, b.hashErr
	}
	return append([]int64(nil), b.hashes[hash]...), nil
}

func (b *fakeBackend) Transcript(_ context.Context, id sre.UUID,
	keep bool) (sre.TranscriptView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transcriptKeep = append(b.transcriptKeep, keep)
	if b.transcriptErr != nil {
		return sre.TranscriptView{}, b.transcriptErr
	}
	v, ok := b.transcripts[id]
	if !ok {
		return sre.TranscriptView{}, fmt.Errorf("%w: %s", sre.ErrNoTranscript, id)
	}
	return v, nil
}
