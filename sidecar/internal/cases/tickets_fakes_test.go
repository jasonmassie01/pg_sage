package cases

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// memTicketStore is an in-memory TicketStore for the service and syncer
// unit tests (market-14). It enforces the unique (database, case, system)
// key the real table enforces.
type memTicketStore struct {
	mu       sync.Mutex
	next     int64
	rows     map[int64]*Ticket
	dueLimit int
	syncs    int
	err      error
}

func newMemTicketStore() *memTicketStore {
	return &memTicketStore{rows: map[int64]*Ticket{}}
}

func (m *memTicketStore) Claim(_ context.Context, c TicketClaim) (Ticket, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return Ticket{}, false, m.err
	}
	for _, t := range m.rows {
		if t.Database == c.Database && t.CaseIdentityKey == c.CaseIdentityKey &&
			t.System == c.System {
			return *t, false, nil
		}
	}
	m.next++
	ch := c.ChannelID
	t := &Ticket{ID: m.next, Database: c.Database, CaseIdentityKey: c.CaseIdentityKey,
		System: c.System, ChannelID: &ch, Status: "creating", CreatedBy: c.CreatedBy,
		CreatedAt: time.Now()}
	m.rows[t.ID] = t
	return *t, true, nil
}

func (m *memTicketStore) Complete(_ context.Context, id int64, ref IssueRef) (Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.rows[id]
	if !ok {
		return Ticket{}, errors.New("no such ticket")
	}
	t.ExternalKey, t.URL, t.Status = ref.Key, ref.URL, ""
	return *t, nil
}

func (m *memTicketStore) Release(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

func (m *memTicketStore) List(_ context.Context, database string) ([]Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Ticket{}
	for _, t := range m.rows {
		if database == "" || database == "all" || t.Database == database {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memTicketStore) DueForSync(
	_ context.Context, limit int, _ time.Duration,
) ([]Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dueLimit = limit
	if m.err != nil {
		return nil, m.err
	}
	var out []Ticket
	for _, t := range m.rows {
		if t.ExternalKey != "" && len(out) < limit {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memTicketStore) RecordSync(
	_ context.Context, id int64, st IssueStatus, syncErr string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncs++
	t := m.rows[id]
	if syncErr == "" {
		t.Status, t.StatusCategory = st.Name, st.Category
	}
	t.SyncError = syncErr
	now := time.Now()
	t.LastSyncedAt = &now
	return nil
}

func (m *memTicketStore) ClaimResolvedComment(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.rows[id]
	if t.ResolvedCommentAt != nil {
		return false, nil
	}
	now := time.Now()
	t.ResolvedCommentAt = &now
	return true, nil
}

func (m *memTicketStore) ReleaseResolvedComment(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id].ResolvedCommentAt = nil
	return nil
}

func (m *memTicketStore) get(id int64) Ticket {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.rows[id]
}

// fakeTracker is a scripted ticket system.
type fakeTracker struct {
	mu        sync.Mutex
	created   []IssueRequest
	createErr error
	status    IssueStatus
	statusErr error
	comments  []string
	commentOK error
}

func (f *fakeTracker) CreateIssue(_ context.Context, req IssueRequest) (IssueRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return IssueRef{}, f.createErr
	}
	f.created = append(f.created, req)
	return IssueRef{Key: "OPS-7", URL: "https://jira.example.com/browse/OPS-7"}, nil
}

func (f *fakeTracker) IssueStatus(_ context.Context, key string) (IssueStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.statusErr
}

func (f *fakeTracker) AddComment(_ context.Context, key, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentOK != nil {
		return f.commentOK
	}
	f.comments = append(f.comments, key+": "+text)
	return nil
}

func (f *fakeTracker) set(st IssueStatus, err error) {
	f.mu.Lock()
	f.status, f.statusErr = st, err
	f.mu.Unlock()
}

func trackerOf(f *fakeTracker) TrackerFunc {
	return func(context.Context, int) (TicketTracker, error) { return f, nil }
}

func ticketCase() Case {
	return Case{ID: "c1", DatabaseName: "db1", IdentityKey: "finding:db1:bloat:table:t",
		Title: "Table t is bloated", Severity: SeverityWarning, State: StateOpen,
		Why: "dead tuples are 60%", WhyNow: "grew 10% this week"}
}
