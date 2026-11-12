package discovery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func enabledCfg() config.FleetDiscoveryConfig {
	return config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 10, TTLHours: 72}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time      { return c.now }
func (c *clock) add(d time.Duration) { c.now = c.now.Add(d) }
func newClock(at time.Time) *clock   { return &clock{now: at} }
func (c *clock) set(at time.Time)    { c.now = at }
func mustRunner(t *testing.T, cfg config.FleetDiscoveryConfig, b []Binding,
	st Store, m Members, c *clock) *Runner {
	t.Helper()
	r, err := NewRunner(cfg, b, st, m)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r.WithClock(c.Now)
}

// CHECK-03: discovery disabled (the default) makes no listing call.
func TestRunner_DisabledMakesNoListingCalls(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	st, m := newFakeStore(), &fakeMembers{}
	cfg := config.DefaultConfig().Fleet.Discovery
	r := mustRunner(t, cfg, []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}}, st, m,
		newClock(t0))
	for range 3 {
		rep := r.Cycle(context.Background())
		if len(rep.Sources) != 0 {
			t.Fatalf("disabled cycle reported sources: %+v", rep.Sources)
		}
	}
	if n := src.callCount(); n != 0 {
		t.Fatalf("listing calls with discovery disabled = %d, want 0", n)
	}
	if st.upserts != 0 || len(m.enrolled) != 0 {
		t.Fatalf("disabled runner wrote: upserts=%d enrolled=%v", st.upserts, m.enrolled)
	}
}

func TestNewRunner_NilDependenciesWhenEnabled(t *testing.T) {
	src := &fakeSource{name: "c"}
	b := []Binding{{Source: src, Owner: "o"}}
	if _, err := NewRunner(enabledCfg(), b, nil, &fakeMembers{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewRunner(enabledCfg(), b, newFakeStore(), nil); err == nil {
		t.Fatal("nil members accepted")
	}
	if _, err := NewRunner(enabledCfg(), []Binding{{Source: src}}, newFakeStore(),
		&fakeMembers{}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("binding without owner: %v", err)
	}
	if _, err := NewRunner(enabledCfg(), []Binding{{Owner: "o"}}, newFakeStore(),
		&fakeMembers{}); err == nil {
		t.Fatal("binding without a source accepted")
	}
	r, err := NewRunner(config.FleetDiscoveryConfig{}, nil, nil, nil)
	if err != nil || r == nil {
		t.Fatalf("disabled runner with no deps should build: %v", err)
	}
}

func TestRunner_HappyPathRecordsCandidatesAndEnrols(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a"), cand("b")}}}
	st, m := newFakeStore(), &fakeMembers{}
	c := newClock(t0)
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "team",
		EnrolFrom: "main"}}, st, m, c)
	rep := r.Cycle(context.Background())
	sr := rep.Sources["c"]
	if sr.Err != nil || sr.Listed != 2 || sr.New != 2 || sr.Gone != 0 {
		t.Fatalf("source report = %+v", sr)
	}
	if rep.Enrolled != 2 || len(m.enrolled) != 2 || m.enrolled[0] != "main/a" {
		t.Fatalf("enrolled = %d %v", rep.Enrolled, m.enrolled)
	}
	want := t0.Add(72 * time.Hour)
	if !m.expires[0].Equal(want) {
		t.Fatalf("expires_at = %v, want %v", m.expires[0], want)
	}
	if got := st.byExternal("c", "a"); got.Status != store.DiscoveryEnrolled ||
		got.Owner != "team" || got.DatabaseID == nil {
		t.Fatalf("row a = %+v", got)
	}
	if len(st.runs["c"]) != 1 || st.runs["c"][0] != nil {
		t.Fatalf("source run records = %v", st.runs["c"])
	}
}

func TestRunner_CandidatesOnlyBindingNeverEnrols(t *testing.T) {
	src := &fakeSource{name: "rds", results: [][]Candidate{{cand("db-1")}}}
	st, m := newFakeStore(), &fakeMembers{}
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o"}}, st, m,
		newClock(t0))
	rep := r.Cycle(context.Background())
	if rep.Enrolled != 0 || len(m.enrolled) != 0 {
		t.Fatalf("candidates-only source enrolled %v", m.enrolled)
	}
	if got := st.byExternal("rds", "db-1"); got.Status != store.DiscoveryCandidate {
		t.Fatalf("row status = %q, want candidate", got.Status)
	}
}

func TestRunner_EmptyListingMarksNothingGoneOnFirstRun(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{}}}
	st, m := newFakeStore(), &fakeMembers{}
	// A row left by an earlier process: the first empty listing must not
	// mark it gone.
	_, _ = st.Upsert(context.Background(), "c", "o", []Candidate{cand("old")},
		t0.Add(-time.Hour))
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o"}}, st, m,
		newClock(t0))
	rep := r.Cycle(context.Background())
	if rep.Sources["c"].Gone != 0 || st.goneRuns != 0 {
		t.Fatalf("first run marked gone: report=%+v goneRuns=%d",
			rep.Sources["c"], st.goneRuns)
	}
	if got := st.byExternal("c", "old"); got.Status != store.DiscoveryCandidate {
		t.Fatalf("old row = %q, want candidate", got.Status)
	}
}

func TestRunner_SecondRunMarksAbsentGoneAndDisablesMember(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{
		{cand("a"), cand("b")}, {cand("a")}}}
	st, m := newFakeStore(), &fakeMembers{}
	c := newClock(t0)
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o",
		EnrolFrom: "main"}}, st, m, c)
	r.Cycle(context.Background())
	b := st.byExternal("c", "b")
	c.add(10 * time.Minute)
	rep := r.Cycle(context.Background())
	if rep.Sources["c"].Gone != 1 {
		t.Fatalf("gone = %d, want 1", rep.Sources["c"].Gone)
	}
	if got := st.byExternal("c", "b"); got.Status != store.DiscoveryGone {
		t.Fatalf("b status = %q, want gone", got.Status)
	}
	if len(m.disabled) != 1 || m.disabled[0] != *b.DatabaseID {
		t.Fatalf("disabled = %v, want [%d]", m.disabled, *b.DatabaseID)
	}
	if got := st.byExternal("c", "a"); got.Status != store.DiscoveryEnrolled {
		t.Fatalf("a status = %q, want enrolled", got.Status)
	}
}

func TestRunner_TruncatedListingMarksNothingGone(t *testing.T) {
	full := []Candidate{cand("a"), cand("b"), cand("c")}
	src := &fakeSource{name: "c", results: [][]Candidate{full, full[:2]}}
	st, m := newFakeStore(), &fakeMembers{}
	c := newClock(t0)
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o"}}, st, m, c)
	r = r.WithCap(2)
	r.Cycle(context.Background())
	c.add(time.Minute)
	rep := r.Cycle(context.Background())
	if !rep.Sources["c"].Truncated || rep.Sources["c"].Gone != 0 {
		t.Fatalf("capped listing report = %+v", rep.Sources["c"])
	}
	if st.goneRuns != 0 {
		t.Fatalf("a listing at the cap ran gone marking %d times", st.goneRuns)
	}
}

// An RDS 403 keeps the previous candidates and records source_error.
func TestRunner_SourceErrorKeepsPreviousCandidates(t *testing.T) {
	denied := errors.New("RDS DescribeDBInstances: api error AccessDenied: 403")
	src := &fakeSource{name: "rds", results: [][]Candidate{{cand("db-1")}},
		errs: []error{nil, denied}}
	st, m := newFakeStore(), &fakeMembers{}
	c := newClock(t0)
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o"}}, st, m, c)
	r.Cycle(context.Background())
	c.add(time.Hour)
	rep := r.Cycle(context.Background())
	if !errors.Is(rep.Sources["rds"].Err, denied) {
		t.Fatalf("report error = %v, want the 403", rep.Sources["rds"].Err)
	}
	if st.goneRuns != 0 {
		t.Fatal("a failed listing ran gone marking")
	}
	if got := st.byExternal("rds", "db-1"); got.Status != store.DiscoveryCandidate {
		t.Fatalf("candidate after 403 = %q", got.Status)
	}
	runs := st.runs["rds"]
	if len(runs) != 2 || runs[0] != nil || !errors.Is(runs[1], denied) {
		t.Fatalf("source runs = %v, want [nil, 403]", runs)
	}
}

func TestRunner_EnrolmentStopsAtMaxEnrolled(t *testing.T) {
	cs := []Candidate{cand("a"), cand("b"), cand("c"), cand("d")}
	src := &fakeSource{name: "c", results: [][]Candidate{cs}}
	st, m := newFakeStore(), &fakeMembers{}
	cfg := enabledCfg()
	cfg.MaxEnrolled = 3
	c := newClock(t0)
	r := mustRunner(t, cfg, []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}}, st, m, c)
	for range 3 {
		r.Cycle(context.Background())
		c.add(time.Minute)
	}
	if len(m.enrolled) != 3 {
		t.Fatalf("enrolled %d members, want max_enrolled=3: %v", len(m.enrolled), m.enrolled)
	}
	if got := st.byExternal("c", "d"); got.Status != store.DiscoveryCandidate {
		t.Fatalf("4th row = %q, want candidate", got.Status)
	}
}

func TestRunner_MaxEnrolledZeroIsCandidatesOnly(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	st, m := newFakeStore(), &fakeMembers{}
	cfg := enabledCfg()
	cfg.MaxEnrolled = 0
	r := mustRunner(t, cfg, []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}}, st, m,
		newClock(t0))
	if rep := r.Cycle(context.Background()); rep.Enrolled != 0 || len(m.enrolled) != 0 {
		t.Fatalf("max_enrolled 0 enrolled %v", m.enrolled)
	}
}

func TestRunner_EnrolFailureReleasesTheClaim(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	st := newFakeStore()
	m := &fakeMembers{enrolErr: errors.New("connection refused")}
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}},
		st, m, newClock(t0))
	rep := r.Cycle(context.Background())
	if rep.Enrolled != 0 || len(rep.Errors) != 1 ||
		!strings.Contains(rep.Errors[0].Error(), "connection refused") {
		t.Fatalf("report = %+v", rep)
	}
	got := st.byExternal("c", "a")
	if got.Status != store.DiscoveryCandidate ||
		!strings.Contains(got.LastError, "connection refused") {
		t.Fatalf("released row = %+v", got)
	}
}

func TestRunner_ClaimErrorIsReported(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	st, m := newFakeStore(), &fakeMembers{}
	st.claimErr = errors.New("permission denied for table discovered_databases")
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}},
		st, m, newClock(t0))
	rep := r.Cycle(context.Background())
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0].Error(), "permission denied") {
		t.Fatalf("claim error not reported: %+v", rep.Errors)
	}
}

// Boundary: at expires_at exactly the member is disabled; a second before
// it is not.
func TestRunner_ExpiryAtExactlyExpiresAt(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	st, m := newFakeStore(), &fakeMembers{}
	c := newClock(t0)
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}},
		st, m, c)
	r.Cycle(context.Background())
	exp := m.expires[0]
	row := st.byExternal("c", "a")
	_ = st.update(row.ID, func(r *store.DiscoveredRow) error { r.ExpiresAt = &exp; return nil })
	c.set(exp.Add(-time.Second))
	if rep := r.Cycle(context.Background()); rep.Expired != 0 || len(m.disabled) != 0 {
		t.Fatalf("expired a second early: %+v %v", rep, m.disabled)
	}
	c.set(exp)
	rep := r.Cycle(context.Background())
	if rep.Expired != 1 || len(m.disabled) != 1 || m.disabled[0] != *row.DatabaseID {
		t.Fatalf("at expires_at: expired=%d disabled=%v", rep.Expired, m.disabled)
	}
	if got := st.byExternal("c", "a"); got.Status != store.DiscoveryExpired {
		t.Fatalf("status = %q, want expired", got.Status)
	}
}

// State transitions: trust stays observation however many cycles run; the
// runner has no path that changes it.
func TestRunner_TrustStaysObservationAfterTenCycles(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a"), cand("b")}}}
	st, m := newFakeStore(), &fakeMembers{}
	c := newClock(t0)
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}},
		st, m, c)
	for range 10 {
		r.Cycle(context.Background())
		c.add(10 * time.Minute)
	}
	if len(m.enrolled) != 2 {
		t.Fatalf("enrolled %v, want each candidate once", m.enrolled)
	}
	for id, trust := range m.trust {
		if trust != "observation" {
			t.Fatalf("member %d trust = %q", id, trust)
		}
	}
}

// Concurrency: overlapping cycles of one runner are serialised, so a
// candidate is enrolled once.
func TestRunner_ConcurrentCyclesEnrolOnce(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	st, m := newFakeStore(), &fakeMembers{}
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}},
		st, m, newClock(t0))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); r.Cycle(context.Background()) }()
	}
	wg.Wait()
	if len(m.enrolled) != 1 {
		t.Fatalf("enrolled %v, want one", m.enrolled)
	}
}

func TestRun_StopsWhenContextEnds(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o"}}, newFakeStore(),
		&fakeMembers{}, newClock(t0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for src.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if src.callCount() != 1 {
		t.Fatalf("Run listed %d times before the first interval, want 1", src.callCount())
	}
}

// Error propagation: a member whose enrolment cannot be recorded is
// disabled again, so no unlinked member outlives its TTL.
func TestRunner_MarkEnrolledFailureDisablesTheMember(t *testing.T) {
	src := &fakeSource{name: "c", results: [][]Candidate{{cand("a")}}}
	st, m := newFakeStore(), &fakeMembers{}
	st.markErr = errors.New("discovered database 1 is not enrolling")
	r := mustRunner(t, enabledCfg(), []Binding{{Source: src, Owner: "o", EnrolFrom: "m"}},
		st, m, newClock(t0))
	rep := r.Cycle(context.Background())
	if rep.Enrolled != 0 || len(rep.Errors) != 1 ||
		!strings.Contains(rep.Errors[0].Error(), "member 1 disabled again") {
		t.Fatalf("report = %+v", rep)
	}
	if len(m.enrolled) != 1 || len(m.disabled) != 1 || m.disabled[0] != 1 {
		t.Fatalf("enrolled %v, disabled %v; want member 1 enrolled then disabled",
			m.enrolled, m.disabled)
	}
}
