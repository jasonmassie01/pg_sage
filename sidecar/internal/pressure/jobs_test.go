package pressure

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// guard-01b without a server: job specs, identity keys and the enforcer's
// arithmetic (two consecutive stops propose once, per backend).

func TestPressureJobSpec_Validate(t *testing.T) {
	ok := JobSpec{Database: "orders", ApplicationName: "backfill_orders",
		RegisteredBy: "user:1"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid spec refused: %v", err)
	}
	cases := map[string]func(*JobSpec){
		"no database":          func(s *JobSpec) { s.Database = " " },
		"no application name":  func(s *JobSpec) { s.ApplicationName = "" },
		"name over 63 bytes":   func(s *JobSpec) { s.ApplicationName = strings.Repeat("a", 64) },
		"non-printable name":   func(s *JobSpec) { s.ApplicationName = "job\n1" },
		"non-ASCII name":       func(s *JobSpec) { s.ApplicationName = "jöb" },
		"pg_sage's own name":   func(s *JobSpec) { s.ApplicationName = "my_PG_SAGE_job" },
		"no registrant":        func(s *JobSpec) { s.RegisteredBy = "" },
		"negative ttl":         func(s *JobSpec) { s.TTL = -time.Second },
		"ttl over the maximum": func(s *JobSpec) { s.TTL = MaxJobTTL + time.Second },
		"table over 260":       func(s *JobSpec) { s.Table = strings.Repeat("t", 261) },
	}
	for name, mutate := range cases {
		s := ok
		mutate(&s)
		err := s.Validate()
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: Validate = %v, want ErrInvalidRequest", name, err)
		}
	}
	// Boundary: 63 bytes and exactly the maximum TTL are accepted.
	edge := ok
	edge.ApplicationName, edge.TTL = strings.Repeat("a", 63), MaxJobTTL
	if err := edge.Validate(); err != nil {
		t.Fatalf("63-byte name with the maximum TTL refused: %v", err)
	}
	if got := (JobSpec{}).ttl(); got != DefaultJobTTL {
		t.Fatalf("zero TTL = %s, want the default %s", got, DefaultJobTTL)
	}
}

func TestPressureJobIdentityKey_RoundTrips(t *testing.T) {
	start := time.Date(2026, 11, 17, 10, 4, 5, 123456000, time.UTC)
	key := JobIdentityKey(42, 31337, start)
	if !strings.HasPrefix(key, JobIdentityPrefix) {
		t.Fatalf("key %q lacks the prefix %q", key, JobIdentityPrefix)
	}
	job, pid, got, ok := ParseJobIdentityKey(key)
	if !ok || job != 42 || pid != 31337 || !got.Equal(start) {
		t.Fatalf("parse(%q) = %d %d %s %v", key, job, pid, got, ok)
	}
	for _, bad := range []string{"", "sre_proposal:x", JobIdentityPrefix,
		JobIdentityPrefix + "1:2", JobIdentityPrefix + "a:2:3", JobIdentityPrefix + "1:-2:3",
		JobIdentityPrefix + "0:2:3", JobIdentityPrefix + "1:2:x", JobIdentityPrefix + "1:2:3:4"} {
		if _, _, _, ok := ParseJobIdentityKey(bad); ok {
			t.Errorf("parse(%q) accepted a malformed key", bad)
		}
	}
}

// --- enforcer fakes ---

type fakeJobs struct {
	mu   sync.Mutex
	jobs []Job
	err  error
}

func (f *fakeJobs) Active(context.Context, string) ([]Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Job(nil), f.jobs...), f.err
}

func (f *fakeJobs) Get(_ context.Context, id int64) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return Job{}, ErrJobNotFound
}

type fakeVerdicts struct {
	mu    sync.Mutex
	next  []Kind
	err   error
	calls []Request
	at    time.Time
}

func (fv *fakeVerdicts) Check(_ context.Context, req Request) (Verdict, error) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	fv.calls = append(fv.calls, req)
	if fv.err != nil {
		return Verdict{}, fv.err
	}
	k := Stop
	if len(fv.next) > 0 {
		k, fv.next = fv.next[0], fv.next[1:]
	}
	fv.at = fv.at.Add(time.Second)
	return Verdict{Database: req.Database, Table: req.Table, Job: req.Job, Verdict: k,
		CheckedAt: fv.at, Reasons: []string{"replica_lag 16.8 s >= stop 15 s"},
		Inputs: []Input{{Name: "replica_lag", State: StateKnown, Value: f(16.8),
			Unit: "s", StopLimit: f(15), Band: BandStop}}}, nil
}

type fakeBackends struct {
	mu      sync.Mutex
	writing []Backend
	err     error
	recheck *Backend
}

func (f *fakeBackends) Writing(context.Context, string) ([]Backend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Backend(nil), f.writing...), f.err
}

func (f *fakeBackends) Recheck(_ context.Context, _ string, pid int32,
	start time.Time) (Backend, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Backend{}, false, f.err
	}
	if f.recheck == nil || f.recheck.PID != pid || !f.recheck.BackendStart.Equal(start) {
		return Backend{}, false, nil
	}
	return *f.recheck, true, nil
}

type fakeProposer struct {
	mu    sync.Mutex
	calls []Proposal
	err   error
	keys  map[string]int
}

func (f *fakeProposer) Propose(_ context.Context, p Proposal) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, false, f.err
	}
	f.calls = append(f.calls, p)
	if f.keys == nil {
		f.keys = map[string]int{}
	}
	key := JobIdentityKey(p.Job.ID, p.Backend.PID, p.Backend.BackendStart)
	if id, ok := f.keys[key]; ok {
		return id, false, nil
	}
	f.keys[key] = len(f.keys) + 1
	return f.keys[key], true, nil
}

func (f *fakeProposer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var fixtureStart = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

func writingBackend(pid int32) Backend {
	return Backend{PID: pid, BackendStart: fixtureStart, QueryStart: fixtureStart.Add(time.Minute),
		Database: "orders", User: "app", ApplicationName: "backfill_orders",
		QueryHash: strings.Repeat("ab", 32), ObservedAt: fixtureStart.Add(2 * time.Minute)}
}

func enforceJob(enforce bool) Job {
	return Job{ID: 7, Database: "orders", ApplicationName: "backfill_orders",
		Table: "public.orders", RegisteredBy: "user:1", Enforce: enforce,
		CreatedAt: fixtureStart, ExpiresAt: fixtureStart.Add(time.Hour)}
}

type enforcerRig struct {
	jobs      *fakeJobs
	verdicts  *fakeVerdicts
	backends  *fakeBackends
	proposals *fakeProposer
	e         *Enforcer
}

func newEnforcerRig(t *testing.T, jobs ...Job) *enforcerRig {
	t.Helper()
	r := &enforcerRig{jobs: &fakeJobs{jobs: jobs}, verdicts: &fakeVerdicts{at: fixtureStart},
		backends:  &fakeBackends{writing: []Backend{writingBackend(4242)}},
		proposals: &fakeProposer{}}
	e, err := NewEnforcer(EnforcerOptions{Database: "orders", Jobs: r.jobs,
		Pressure: r.verdicts, Backends: r.backends, Proposals: r.proposals,
		Now: func() time.Time { return fixtureStart.Add(5 * time.Minute) }})
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	r.e = e
	return r
}

func TestPressureJobEnforcer_NewRefusesMissingParts(t *testing.T) {
	full := EnforcerOptions{Database: "orders", Jobs: &fakeJobs{}, Pressure: &fakeVerdicts{},
		Backends: &fakeBackends{}, Proposals: &fakeProposer{}}
	for name, mutate := range map[string]func(*EnforcerOptions){
		"no database":  func(o *EnforcerOptions) { o.Database = "" },
		"no jobs":      func(o *EnforcerOptions) { o.Jobs = nil },
		"no pressure":  func(o *EnforcerOptions) { o.Pressure = nil },
		"no backends":  func(o *EnforcerOptions) { o.Backends = nil },
		"no proposals": func(o *EnforcerOptions) { o.Proposals = nil },
		"one stop":     func(o *EnforcerOptions) { o.Stops = 1 },
	} {
		o := full
		mutate(&o)
		if _, err := NewEnforcer(o); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: NewEnforcer = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// Nil/empty: no registered jobs read no pressure and propose nothing.
func TestPressureJobEnforcer_NoJobsNoProposals(t *testing.T) {
	r := newEnforcerRig(t)
	res, err := r.e.Tick(context.Background())
	if err != nil || res.Checked != 0 || res.Proposed != 0 {
		t.Fatalf("tick = %+v, %v; want nothing checked or proposed", res, err)
	}
	if len(r.verdicts.calls) != 0 || r.proposals.count() != 0 {
		t.Fatalf("no jobs still read pressure %d times, proposed %d", len(r.verdicts.calls),
			r.proposals.count())
	}
}

// A job registered without enforce is never checked by the enforcer.
func TestPressureJobEnforcer_UnenforcedJobIsNotChecked(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(false))
	for i := 0; i < 3; i++ {
		if _, err := r.e.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if len(r.verdicts.calls) != 0 || r.proposals.count() != 0 {
		t.Fatalf("unenforced job checked %d times, proposed %d", len(r.verdicts.calls),
			r.proposals.count())
	}
}

// Boundary: one stop does not propose; the second consecutive one does,
// with the job's table and application name on the check and the verdict
// on the proposal.
func TestPressureJobEnforcer_OneStopDoesNotProposeTwoDo(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	ctx := context.Background()
	res, err := r.e.Tick(ctx)
	if err != nil || res.Checked != 1 || res.Proposed != 0 || r.proposals.count() != 0 {
		t.Fatalf("first stop: %+v %v, %d proposals; want none", res, err, r.proposals.count())
	}
	res, err = r.e.Tick(ctx)
	if err != nil || res.Proposed != 1 || r.proposals.count() != 1 {
		t.Fatalf("second stop: %+v %v, %d proposals; want one", res, err, r.proposals.count())
	}
	call := r.verdicts.calls[1]
	if call.Table != "public.orders" || call.Job != "backfill_orders" || call.Database != "orders" {
		t.Fatalf("check request = %+v, want the job's table and name", call)
	}
	p := r.proposals.calls[0]
	if p.Job.ID != 7 || p.Backend.PID != 4242 || p.Stops != 2 || p.Verdict.Verdict != Stop ||
		len(p.Verdict.Inputs) != 1 {
		t.Fatalf("proposal = %+v", p)
	}
	if want := fixtureStart.Add(5*time.Minute + DefaultApprovalTTL); !p.ExpiresAt.Equal(want) {
		t.Fatalf("proposal expires %s, want %s", p.ExpiresAt, want)
	}
}

// A stop interrupted by slow starts over: stop, slow, stop is not two.
func TestPressureJobEnforcer_NonStopResetsTheStreak(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	r.verdicts.next = []Kind{Stop, Slow, Stop, Proceed, Stop}
	for i := 0; i < 5; i++ {
		if _, err := r.e.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if r.proposals.count() != 0 {
		t.Fatalf("interrupted stops proposed %d times", r.proposals.count())
	}
}

// The same cached read set seen twice counts once.
func TestPressureJobEnforcer_SameReadSetCountsOnce(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	v, _ := r.verdicts.Check(context.Background(), Request{})
	for i := 0; i < 3; i++ {
		if _, err := r.e.Observe(context.Background(), enforceJob(true), v); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	if r.proposals.count() != 0 {
		t.Fatalf("one cached stop seen three times proposed %d times", r.proposals.count())
	}
}

// A stopped job whose backend is not writing (idle between batches, or
// gone) gets no proposal.
func TestPressureJobEnforcer_NoWritingBackendNoProposal(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	r.backends.writing = nil
	for i := 0; i < 3; i++ {
		if _, err := r.e.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if r.proposals.count() != 0 {
		t.Fatalf("no writing backend still proposed %d times", r.proposals.count())
	}
}

// Error propagation: a failed check, backend read or proposal names the
// job; the failed check proposes nothing and does not count as a stop.
func TestPressureJobEnforcer_ErrorsNameTheJob(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	r.verdicts.err = errors.New("pressure inputs unreadable")
	for i := 0; i < 2; i++ {
		_, err := r.e.Tick(context.Background())
		if err == nil || !strings.Contains(err.Error(), "backfill_orders") ||
			!strings.Contains(err.Error(), "pressure inputs unreadable") {
			t.Fatalf("tick error = %v, want the job and the cause", err)
		}
	}
	r.verdicts.err = nil
	if _, err := r.e.Tick(context.Background()); err != nil || r.proposals.count() != 0 {
		t.Fatalf("a failed check counted as a stop: %v, %d proposals", err, r.proposals.count())
	}
	r.backends.err = errors.New("permission denied for pg_stat_activity")
	_, err := r.e.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") ||
		!strings.Contains(err.Error(), "backfill_orders") {
		t.Fatalf("backend read error = %v", err)
	}
	r.backends.err, r.proposals.err = nil, errors.New("queue insert failed")
	if _, err = r.e.Tick(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "queue insert failed") {
		t.Fatalf("proposal error = %v", err)
	}
	r.jobs.err = errors.New("relation sage.pressure_jobs does not exist")
	if _, err = r.e.Tick(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "pressure_jobs") {
		t.Fatalf("job list error = %v", err)
	}
}

// Concurrency: many concurrent stops for one job propose once per backend.
func TestPressureJobEnforcer_ConcurrentStopsProposeOnce(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.e.Tick(context.Background()); err != nil {
				t.Errorf("tick: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := r.proposals.count(); n != 1 {
		t.Fatalf("16 concurrent stops called Propose %d times, want 1", n)
	}
}

// Several writing backends each get their own proposal, capped.
func TestPressureJobEnforcer_EachWritingBackendCapped(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	r.backends.writing = nil
	for pid := int32(100); pid < 110; pid++ {
		r.backends.writing = append(r.backends.writing, writingBackend(pid))
	}
	for i := 0; i < 2; i++ {
		if _, err := r.e.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if n := r.proposals.count(); n != DefaultMaxBackends {
		t.Fatalf("10 writing backends proposed %d times, want the cap %d", n,
			DefaultMaxBackends)
	}
}

// State transitions: registered -> stop -> proposed; the job then
// expires (drops out of the active list) and nothing more is proposed for
// a new backend.
func TestPressureJobEnforcer_ExpiredJobStopsProposing(t *testing.T) {
	r := newEnforcerRig(t, enforceJob(true))
	for i := 0; i < 2; i++ {
		if _, err := r.e.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if r.proposals.count() != 1 {
		t.Fatalf("proposed %d times before expiry, want 1", r.proposals.count())
	}
	r.jobs.mu.Lock()
	r.jobs.jobs = nil
	r.jobs.mu.Unlock()
	r.backends.writing = []Backend{writingBackend(5151)}
	for i := 0; i < 3; i++ {
		if _, err := r.e.Tick(context.Background()); err != nil {
			t.Fatalf("tick after expiry: %v", err)
		}
	}
	if r.proposals.count() != 1 {
		t.Fatalf("an expired job proposed again (%d)", r.proposals.count())
	}
}
