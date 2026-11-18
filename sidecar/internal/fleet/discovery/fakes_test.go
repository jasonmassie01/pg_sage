package discovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// fakeSource returns a scripted listing per call and counts the calls.
type fakeSource struct {
	name    string
	mu      sync.Mutex
	calls   int
	results [][]Candidate
	errs    []error
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) List(context.Context) ([]Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	if len(f.results) == 0 {
		return nil, nil
	}
	return f.results[len(f.results)-1], nil
}

func (f *fakeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func cand(id string) Candidate {
	return Candidate{ExternalID: id, Host: "h", Port: 5432, DBName: id}
}

// fakeStore is an in-memory sage.discovered_databases with the store's
// status rules, so the runner is tested against the same transitions.
type fakeStore struct {
	mu       sync.Mutex
	nextID   int64
	rows     map[string]*store.DiscoveredRow
	runs     map[string][]error
	audits   []string
	upserts  int
	goneRuns int
	claimErr error
	markErr  error
	// fleetpol-01b: the members whose expiry DueForExpiry reads, and the
	// scripted EnrollStats answer.
	expiryFrom *fakeMembers
	stats      []store.EnrollStat
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]*store.DiscoveredRow{}, runs: map[string][]error{}}
}

func (s *fakeStore) key(source, ext string) string { return source + "|" + ext }

func (s *fakeStore) Upsert(_ context.Context, source, owner string, cands []Candidate,
	now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts++
	added := 0
	for _, c := range cands {
		k := s.key(source, c.ExternalID)
		if r, ok := s.rows[k]; ok {
			r.LastSeen, r.Owner = now, owner
			r.Tags, r.Creator = c.Tags, c.Creator
			if r.Status == store.DiscoveryCandidate || r.Status == store.DiscoveryEnrolling {
				r.SetName = c.SetName
			}
			continue
		}
		s.nextID++
		s.rows[k] = &store.DiscoveredRow{ID: s.nextID, Source: source,
			ExternalID: c.ExternalID, Host: c.Host, Port: c.Port, DBName: c.DBName,
			Owner: owner, Status: store.DiscoveryCandidate, FirstSeen: now, LastSeen: now,
			Tags: c.Tags, Creator: c.Creator, SetName: c.SetName}
		added++
	}
	return added, nil
}

func (s *fakeStore) MarkGone(_ context.Context, source string,
	before time.Time) ([]store.DiscoveredRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.goneRuns++
	var out []store.DiscoveredRow
	for _, r := range s.sorted() {
		if r.Source != source || !r.LastSeen.Before(before) ||
			r.Status == store.DiscoveryGone || r.Status == store.DiscoveryIgnored {
			continue
		}
		r.Status = store.DiscoveryGone
		out = append(out, *r)
	}
	return out, nil
}

func (s *fakeStore) RecordSourceRun(_ context.Context, source string, _ int,
	runErr error, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[source] = append(s.runs[source], runErr)
	return nil
}

func (s *fakeStore) Claim(_ context.Context, source string, limit, maxLive int,
	_ time.Time) ([]store.DiscoveredRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	live := 0
	for _, r := range s.rows {
		switch r.Status {
		case store.DiscoveryEnrolling, store.DiscoveryEnrolled, store.DiscoveryAdopted:
			live++
		}
	}
	room := min(limit, maxLive-live)
	var out []store.DiscoveredRow
	for _, r := range s.sorted() {
		if room <= 0 {
			break
		}
		if r.Source == source && r.Status == store.DiscoveryCandidate {
			r.Status = store.DiscoveryEnrolling
			out = append(out, *r)
			room--
		}
	}
	return out, nil
}

func (s *fakeStore) MarkEnrolled(_ context.Context, rowID int64, databaseID int) error {
	if s.markErr != nil {
		return s.markErr
	}
	return s.update(rowID, func(r *store.DiscoveredRow) error {
		if r.Status != store.DiscoveryEnrolling {
			return fmt.Errorf("row %d is %s", rowID, r.Status)
		}
		r.Status, r.DatabaseID = store.DiscoveryEnrolled, &databaseID
		s.audits = append(s.audits, fmt.Sprintf("enrol %d", databaseID))
		return nil
	})
}

func (s *fakeStore) Release(_ context.Context, rowID int64, reason string) error {
	return s.update(rowID, func(r *store.DiscoveredRow) error {
		r.Status, r.LastError = store.DiscoveryCandidate, reason
		return nil
	})
}

func (s *fakeStore) DueForExpiry(_ context.Context, now time.Time) ([]store.DiscoveredRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.DiscoveredRow
	for _, r := range s.sorted() {
		exp := r.ExpiresAt
		if exp == nil && s.expiryFrom != nil && r.DatabaseID != nil {
			exp = s.expiryFrom.expiryOf(*r.DatabaseID)
		}
		if r.Status == store.DiscoveryEnrolled && exp != nil && !exp.After(now) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *fakeStore) SetStatus(_ context.Context, rowID int64, status, reason string) error {
	return s.update(rowID, func(r *store.DiscoveredRow) error {
		r.Status, r.LastError = status, reason
		s.audits = append(s.audits, status)
		return nil
	})
}

func (s *fakeStore) update(rowID int64, fn func(*store.DiscoveredRow) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ID == rowID {
			return fn(r)
		}
	}
	return errors.New("no such row")
}

func (s *fakeStore) sorted() []*store.DiscoveredRow {
	out := make([]*store.DiscoveredRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *fakeStore) byExternal(source, ext string) store.DiscoveredRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[s.key(source, ext)]; ok {
		return *r
	}
	return store.DiscoveredRow{}
}

// fakeMembers records enrolments and disables; enrolErr fails Enrol.
type fakeMembers struct {
	mu       sync.Mutex
	nextID   int
	enrolled []string
	expires  []time.Time
	disabled []int
	enrolErr error
	trust    map[int]string
	rows     []store.DiscoveredRow // fleetpol-01b: each enrolled row as passed
}

func (m *fakeMembers) Enrol(_ context.Context, from string, row store.DiscoveredRow,
	expiresAt time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enrolErr != nil {
		return 0, m.enrolErr
	}
	m.nextID++
	m.enrolled = append(m.enrolled, from+"/"+row.ExternalID)
	m.expires = append(m.expires, expiresAt)
	m.rows = append(m.rows, row)
	if m.trust == nil {
		m.trust = map[int]string{}
	}
	m.trust[m.nextID] = "observation"
	return m.nextID, nil
}

func (m *fakeMembers) Disable(_ context.Context, databaseID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disabled = append(m.disabled, databaseID)
	return nil
}
