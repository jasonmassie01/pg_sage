package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01b: the in-memory store's set-mode methods, with the real
// store's rules (claim per source and set within maxLive; milestones keep
// their first time).

func (s *fakeStore) ClaimSet(_ context.Context, source, set string, limit, maxLive int,
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
		if r.Source == source && r.SetName == set && r.Status == store.DiscoveryCandidate {
			r.Status = store.DiscoveryEnrolling
			out = append(out, *r)
			room--
		}
	}
	return out, nil
}

func (s *fakeStore) PendingGrades(context.Context) ([]store.DiscoveredRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.DiscoveredRow
	for _, r := range s.sorted() {
		if (r.Status == store.DiscoveryEnrolled || r.Status == store.DiscoveryAdopted) &&
			r.SetName != "" && r.BaselineAt == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *fakeStore) MarkMilestone(_ context.Context, rowID int64, m store.Milestone,
	at time.Time) error {
	return s.update(rowID, func(r *store.DiscoveredRow) error {
		field := map[store.Milestone]**time.Time{store.MilestoneEnrolled: &r.EnrolledAt,
			store.MilestoneGradeRequested: &r.GradeRequestedAt,
			store.MilestoneFirstGraded:    &r.FirstGradedAt,
			store.MilestoneBaseline:       &r.BaselineAt}[m]
		if field == nil {
			return fmt.Errorf("unknown milestone %q", m)
		}
		if *field == nil {
			at := at
			*field = &at
		}
		return nil
	})
}

func (s *fakeStore) EnrollStats(context.Context, time.Time) ([]store.EnrollStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.EnrollStat(nil), s.stats...), nil
}

func (s *fakeStore) setStatus(source, ext, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[s.key(source, ext)].Status = status
}

type enrolledAs struct {
	owner   string
	expires time.Time
	tags    map[string]string
}

// byExternal is each enrolled row's owner, expiry and tags by external id.
func (m *fakeMembers) byExternal() map[string]enrolledAs {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]enrolledAs{}
	for i, r := range m.rows {
		out[r.ExternalID] = enrolledAs{owner: r.Owner, expires: m.expires[i], tags: r.Tags}
	}
	return out
}

// expiryOf is member id's expiry (ids are 1-based in enrolment order).
func (m *fakeMembers) expiryOf(id int) *time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || id > len(m.expires) {
		return nil
	}
	at := m.expires[id-1]
	return &at
}
