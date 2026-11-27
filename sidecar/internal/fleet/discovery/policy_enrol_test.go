package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01b: enrollment by policy set, against the in-memory store.

var enrolT0 = time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)

func defaultSet() policy.PolicySet {
	return policy.PolicySet{Name: policy.DefaultSetName,
		Selectors: []policy.Selector{{All: true}}}
}

func agentSet() policy.PolicySet {
	return policy.PolicySet{Name: "agent-created", Priority: 20,
		Selectors: []policy.Selector{{Creator: "agent:*"}},
		Requirements: policy.SetRequirements{TTLHours: 24, EnrollmentDeadlineHours: 24,
			Grades: []policy.GradeRequirement{{Dimension: "safety", MinLetter: "B",
				WithinHours: 24}},
			Enrollment: policy.SetEnrollment{IntervalMinutes: 2, Owner: policy.EnrollOwnerCreator}}}
}

// fakeGrades is review-01's fake grader: grades are scripted per member.
type fakeGrades struct {
	mu        sync.Mutex
	requested []string
	notReady  map[string]bool
	grades    map[string][]Grade
	readErr   error
}

func (g *fakeGrades) RequestFirstGrade(_ context.Context, member string,
	_ store.DiscoveredRow) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.notReady[member] {
		return fmt.Errorf("%w: %s has no review queue yet", ErrMemberNotReady, member)
	}
	g.requested = append(g.requested, member)
	return nil
}

func (g *fakeGrades) Grades(_ context.Context, member string) ([]Grade, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.readErr != nil {
		return nil, g.readErr
	}
	return g.grades[member], nil
}

func (g *fakeGrades) add(member string, at time.Time, letters map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.grades == nil {
		g.grades = map[string][]Grade{}
	}
	g.grades[member] = append(g.grades[member], Grade{At: at, Letters: letters})
}

type setsEnv struct {
	r       *Runner
	st      *fakeStore
	members *fakeMembers
	grades  *fakeGrades
	src     *fakeSource
	now     *time.Time
	sets    []policy.PolicySet
	setsErr error
}

func newSetsEnv(t *testing.T, cands []Candidate, sets ...policy.PolicySet) *setsEnv {
	t.Helper()
	now := enrolT0
	e := &setsEnv{st: newFakeStore(), members: &fakeMembers{}, grades: &fakeGrades{},
		src: &fakeSource{name: "cluster", results: [][]Candidate{cands}}, now: &now,
		sets: sets}
	e.st.expiryFrom = e.members
	r, err := NewRunner(config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 50,
		TTLHours: 72}, []Binding{{Source: e.src, Owner: "team-db", EnrolFrom: "main"}},
		e.st, e.members)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	e.r = r.WithClock(func() time.Time { return *e.now }).WithPolicySets(
		func(context.Context) ([]policy.PolicySet, error) { return e.sets, e.setsErr },
		e.grades)
	return e
}

func (e *setsEnv) advance(d time.Duration) { *e.now = e.now.Add(d) }

func agentCand(id, creator string) Candidate {
	c := cand(id)
	c.Creator = creator
	return c
}

// Happy path: an agent-created database is enrolled under agent-created
// with the creator as owner and the set's TTL; a person's database (no
// creator) matches default with the source owner and fleet TTL.
func TestPolicyEnrol_ChoosesTheSetOwnerAndTTL(t *testing.T) {
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:builder"), cand("b")},
		defaultSet(), agentSet())
	rep := e.r.Cycle(context.Background())
	if rep.Enrolled != 2 || len(rep.Errors) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	a, b := e.st.byExternal("cluster", "a"), e.st.byExternal("cluster", "b")
	if a.SetName != "agent-created" || b.SetName != policy.DefaultSetName {
		t.Fatalf("sets: a %q b %q", a.SetName, b.SetName)
	}
	if a.EnrolledAt == nil || !a.EnrolledAt.Equal(enrolT0) {
		t.Fatalf("a enrolled_at = %v", a.EnrolledAt)
	}
	got := e.members.byExternal()
	if got["a"].owner != "agent:builder" || !got["a"].expires.Equal(enrolT0.Add(24*time.Hour)) {
		t.Fatalf("a enrolled as %+v, want owner agent:builder for 24h", got["a"])
	}
	if got["b"].owner != "team-db" || !got["b"].expires.Equal(enrolT0.Add(72*time.Hour)) {
		t.Fatalf("b enrolled as %+v, want the source owner for 72h", got["b"])
	}
	if rep.PerSet["agent-created"] != 1 || rep.PerSet[policy.DefaultSetName] != 1 {
		t.Fatalf("per set = %v", rep.PerSet)
	}
}

// Owner from the tag "owner", falling back to the source owner.
func TestPolicyEnrol_OwnerFromTagFallsBackToTheSource(t *testing.T) {
	set := agentSet()
	set.Requirements.Enrollment.Owner = policy.EnrollOwnerTag
	tagged := agentCand("a", "agent:x")
	tagged.Tags = map[string]string{"owner": "team-agents"}
	e := newSetsEnv(t, []Candidate{tagged, agentCand("b", "agent:y")}, defaultSet(), set)
	e.r.Cycle(context.Background())
	got := e.members.byExternal()
	if got["a"].owner != "team-agents" || got["b"].owner != "team-db" {
		t.Fatalf("owners a %q b %q, want team-agents and team-db", got["a"].owner,
			got["b"].owner)
	}
	if got["a"].tags["owner"] != "team-agents" {
		t.Fatalf("tags did not reach the member: %v", got["a"].tags)
	}
}

// Boundary: per_cycle 5 with 6 candidates enrolls 5, the sixth on the
// set's next cycle; a cycle before the set's interval enrolls nothing.
func TestPolicyEnrol_PerCycleAndInterval(t *testing.T) {
	var cands []Candidate
	for i := range 6 {
		cands = append(cands, cand(fmt.Sprintf("c%d", i)))
	}
	e := newSetsEnv(t, cands, defaultSet())
	if rep := e.r.Cycle(context.Background()); rep.Enrolled != 5 {
		t.Fatalf("first cycle enrolled %d, want 5", rep.Enrolled)
	}
	e.advance(2 * time.Minute)
	if rep := e.r.Cycle(context.Background()); rep.Enrolled != 0 {
		t.Fatalf("2 min later (interval 10) enrolled %d, want 0", rep.Enrolled)
	}
	e.advance(8 * time.Minute)
	if rep := e.r.Cycle(context.Background()); rep.Enrolled != 1 {
		t.Fatalf("10 min later enrolled %d, want the sixth", rep.Enrolled)
	}
}

// A 2-minute set enrolls on every 2-minute cycle; the wait between
// cycles is the shortest set interval.
func TestPolicyEnrol_TwoMinuteSetAndWait(t *testing.T) {
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:x")}, defaultSet(), agentSet())
	if got := e.r.Wait(10 * time.Minute); got != 10*time.Minute {
		t.Fatalf("wait before any cycle = %v, want the default 10m", got)
	}
	e.r.Cycle(context.Background())
	if got := e.r.Wait(10 * time.Minute); got != 2*time.Minute {
		t.Fatalf("wait = %v, want agent-created's 2m", got)
	}
	e.src.results = [][]Candidate{{agentCand("a", "agent:x"), agentCand("b", "agent:x")}}
	e.advance(2 * time.Minute)
	if rep := e.r.Cycle(context.Background()); rep.PerSet["agent-created"] != 1 {
		t.Fatalf("second 2-minute cycle: %+v, want b enrolled", rep)
	}
}

// Nil/empty: no candidates and no sets enroll nothing and fail nothing;
// a candidate with no creator matches default.
func TestPolicyEnrol_NilAndEmpty(t *testing.T) {
	e := newSetsEnv(t, nil, defaultSet(), agentSet())
	if rep := e.r.Cycle(context.Background()); rep.Enrolled != 0 || len(rep.Errors) != 0 {
		t.Fatalf("empty listing: %+v", rep)
	}
	e = newSetsEnv(t, []Candidate{cand("a")})
	rep := e.r.Cycle(context.Background())
	if rep.Enrolled != 0 || len(rep.Errors) != 0 || len(rep.Ignored) == 0 {
		t.Fatalf("no sets: %+v, want nothing enrolled and a reason", rep)
	}
	e = newSetsEnv(t, []Candidate{agentCand("a", "")}, defaultSet(), agentSet())
	e.r.Cycle(context.Background())
	if got := e.st.byExternal("cluster", "a"); got.SetName != policy.DefaultSetName {
		t.Fatalf("no creator matched %q, want default", got.SetName)
	}
}

// Error propagation: a failed set read enrolls nothing (fail closed) but
// the listing is still recorded; a tie between two sets leaves the
// candidate unenrolled with the reason.
func TestPolicyEnrol_SetErrorsFailClosed(t *testing.T) {
	e := newSetsEnv(t, []Candidate{cand("a")}, defaultSet())
	e.setsErr = errors.New("read policy sets: connection refused")
	rep := e.r.Cycle(context.Background())
	if rep.Enrolled != 0 || len(rep.Errors) == 0 ||
		!strings.Contains(fmt.Sprint(rep.Errors), "connection refused") {
		t.Fatalf("set read failure: %+v", rep)
	}
	if e.st.byExternal("cluster", "a").ID == 0 {
		t.Fatal("listing not recorded when sets failed")
	}
	tie := agentSet()
	tie.Name = "agents-two"
	e = newSetsEnv(t, []Candidate{agentCand("a", "agent:x")}, defaultSet(), agentSet(), tie)
	rep = e.r.Cycle(context.Background())
	if rep.Enrolled != 0 || !strings.Contains(strings.Join(rep.Ignored, ";"), "priority") {
		t.Fatalf("tie: %+v, want no enrolment and the tie reason", rep)
	}
}

// Invalid input: a tag key over 64 characters, an empty creator and a
// creator with spaces are ignored with a reason, never stored.
func TestPolicyEnrol_InvalidAttributesIgnoredWithReason(t *testing.T) {
	long := strings.Repeat("k", 65)
	c := agentCand("a", "agent:x")
	c.Tags = map[string]string{long: "v", "tier": "dev"}
	bad := agentCand("b", "agent with space")
	e := newSetsEnv(t, []Candidate{c, bad}, defaultSet(), agentSet())
	rep := e.r.Cycle(context.Background())
	reasons := strings.Join(rep.Ignored, ";")
	if !strings.Contains(reasons, "longer than 64") || !strings.Contains(reasons, "creator") {
		t.Fatalf("reasons = %q", reasons)
	}
	a := e.st.byExternal("cluster", "a")
	if _, ok := a.Tags[long]; ok || a.Tags["tier"] != "dev" || a.SetName != "agent-created" {
		t.Fatalf("a = %+v", a)
	}
	if b := e.st.byExternal("cluster", "b"); b.Creator != "" ||
		b.SetName != policy.DefaultSetName {
		t.Fatalf("b = %+v, want no creator and the default set", b)
	}
	e.advance(10 * time.Minute)
	if rep := e.r.Cycle(context.Background()); len(rep.Ignored) != 0 {
		t.Fatalf("the same reasons were logged again: %v", rep.Ignored)
	}
}

func TestParseDatabaseComment(t *testing.T) {
	for _, tc := range []struct {
		comment, creator string
		tags             map[string]string
		problem          string
	}{
		{"pg_sage: creator=agent:x", "agent:x", map[string]string{}, ""},
		{"pg_sage: creator=agent:x owner=team tier=dev", "agent:x",
			map[string]string{"owner": "team", "tier": "dev"}, ""},
		{"pg_sage: creator= tier=dev", "", map[string]string{"tier": "dev"}, "creator"},
		{"pg_sage: " + strings.Repeat("k", 65) + "=v", "", map[string]string{}, "longer than 64"},
		{"pg_sage: novalue", "", map[string]string{}, "key=value"},
		{"orders database for the shop", "", nil, ""},
		{"", "", nil, ""},
	} {
		creator, tags, problems := parseDatabaseComment(tc.comment)
		if creator != tc.creator || len(tags) != len(tc.tags) {
			t.Errorf("%q: creator %q tags %v", tc.comment, creator, tags)
		}
		for k, v := range tc.tags {
			if tags[k] != v {
				t.Errorf("%q: tag %s = %q, want %q", tc.comment, k, tags[k], v)
			}
		}
		joined := strings.Join(problems, ";")
		if (tc.problem == "") != (joined == "") || !strings.Contains(joined, tc.problem) {
			t.Errorf("%q: problems %q, want %q", tc.comment, joined, tc.problem)
		}
	}
}

// State transitions: discovered -> enrolled -> grade requested -> graded
// (below the set's baseline) -> at baseline -> TTL expired; an adopted
// member is graded but never expired.
func TestPolicyEnrol_StateTransitions(t *testing.T) {
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:x"), agentCand("b", "agent:y")},
		defaultSet(), agentSet())
	e.r.Cycle(context.Background())
	a := e.st.byExternal("cluster", "a")
	member := MemberName(a)
	if a.Status != store.DiscoveryEnrolled || a.GradeRequestedAt == nil ||
		!strings.Contains(strings.Join(e.grades.requested, ","), member) {
		t.Fatalf("after enrolment: %+v requested %v", a, e.grades.requested)
	}
	e.grades.add(member, enrolT0.Add(time.Minute), map[string]string{"safety": "C"})
	e.advance(2 * time.Minute)
	rep := e.r.Cycle(context.Background())
	a = e.st.byExternal("cluster", "a")
	if a.FirstGradedAt == nil || !a.FirstGradedAt.Equal(enrolT0.Add(time.Minute)) ||
		a.BaselineAt != nil || rep.Graded != 1 {
		t.Fatalf("after a C: %+v report %+v", a, rep)
	}
	e.grades.add(member, enrolT0.Add(3*time.Minute), map[string]string{"safety": "A"})
	e.advance(2 * time.Minute)
	e.r.Cycle(context.Background())
	if a = e.st.byExternal("cluster", "a"); a.BaselineAt == nil ||
		!a.BaselineAt.Equal(enrolT0.Add(3*time.Minute)) {
		t.Fatalf("after an A: %+v, want at baseline", a)
	}
	e.st.setStatus("cluster", "b", store.DiscoveryAdopted)
	e.advance(25 * time.Hour)
	e.r.Cycle(context.Background())
	if a = e.st.byExternal("cluster", "a"); a.Status != store.DiscoveryExpired {
		t.Fatalf("after the TTL: %q, want expired", a.Status)
	}
	if b := e.st.byExternal("cluster", "b"); b.Status != store.DiscoveryAdopted {
		t.Fatalf("adopted member: %q", b.Status)
	}
}

// A member whose runtime is not up yet is asked again next cycle, with
// no error; a grade read failure is reported.
func TestPolicyEnrol_GradeRequestRetriesAndReadErrors(t *testing.T) {
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:x")}, defaultSet(), agentSet())
	e.grades.notReady = map[string]bool{"cluster-a-1": true}
	rep := e.r.Cycle(context.Background())
	if a := e.st.byExternal("cluster", "a"); a.GradeRequestedAt != nil || len(rep.Errors) != 0 {
		t.Fatalf("not ready: %+v errors %v", a, rep.Errors)
	}
	e.grades.notReady = nil
	e.advance(2 * time.Minute)
	e.r.Cycle(context.Background())
	if a := e.st.byExternal("cluster", "a"); a.GradeRequestedAt == nil {
		t.Fatal("grade not requested once the member was ready")
	}
	e.grades.readErr = errors.New("read sage.review_run: permission denied")
	e.advance(2 * time.Minute)
	rep = e.r.Cycle(context.Background())
	if !strings.Contains(fmt.Sprint(rep.Errors), "permission denied") {
		t.Fatalf("grade read error not reported: %v", rep.Errors)
	}
}

// A member past the set's deadline without a first grade is reported
// once as overdue.
func TestPolicyEnrol_DeadlineOverdue(t *testing.T) {
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:x")}, defaultSet(), agentSet())
	e.r.Cycle(context.Background())
	e.advance(23 * time.Hour)
	if rep := e.r.Cycle(context.Background()); len(rep.Overdue) != 0 {
		t.Fatalf("overdue before the deadline: %v", rep.Overdue)
	}
	e.advance(2 * time.Hour)
	rep := e.r.Cycle(context.Background())
	if len(rep.Overdue) != 1 || !strings.Contains(rep.Overdue[0], "24h") {
		t.Fatalf("overdue = %v", rep.Overdue)
	}
}

// Concurrency: two runners on one store never enroll a candidate twice.
func TestPolicyEnrol_TwoRunnersOneStore(t *testing.T) {
	var cands []Candidate
	for i := range 8 {
		cands = append(cands, agentCand(fmt.Sprintf("c%d", i), "agent:x"))
	}
	e := newSetsEnv(t, cands, defaultSet(), agentSet())
	r2, err := NewRunner(config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 50,
		TTLHours: 72}, []Binding{{Source: e.src, Owner: "team-db", EnrolFrom: "main"}},
		e.st, e.members)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r2 = r2.WithClock(func() time.Time { return enrolT0 }).WithPolicySets(
		func(context.Context) ([]policy.PolicySet, error) { return e.sets, nil }, e.grades)
	var wg sync.WaitGroup
	for _, r := range []*Runner{e.r, r2, e.r, r2} {
		wg.Add(1)
		go func(r *Runner) { defer wg.Done(); r.Cycle(context.Background()) }(r)
	}
	wg.Wait()
	seen := map[string]int{}
	for _, x := range e.members.enrolled {
		seen[x]++
		if seen[x] > 1 {
			t.Fatalf("%s enrolled twice: %v", x, e.members.enrolled)
		}
	}
}

// Metrics: per-set labels for the enroll time quantiles and the 24 h
// baseline share; nothing before the first cycle.
func TestPolicyEnrol_WritesPerSetMetrics(t *testing.T) {
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:x")}, defaultSet(), agentSet())
	var before strings.Builder
	e.r.WriteMetrics(&before)
	if before.Len() != 0 {
		t.Fatalf("metrics before a cycle: %q", before.String())
	}
	e.st.stats = []store.EnrollStat{{Set: "agent-created", Enrolled: 2, P50Seconds: 90,
		P95Seconds: 150.5, Eligible: 2, AtBaseline: 1}, {Set: "default", Enrolled: 1,
		P50Seconds: 30, P95Seconds: 30}}
	e.r.Cycle(context.Background())
	var b strings.Builder
	e.r.WriteMetrics(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_fleet_enroll_seconds gauge",
		`pg_sage_fleet_enroll_seconds{set="agent-created",quantile="0.5"} 90`,
		`pg_sage_fleet_enroll_seconds{set="agent-created",quantile="0.95"} 150.5`,
		`pg_sage_fleet_enroll_seconds{set="default",quantile="0.5"} 30`,
		"# TYPE pg_sage_fleet_baseline_24h_ratio gauge",
		`pg_sage_fleet_baseline_24h_ratio{set="agent-created"} 0.5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `pg_sage_fleet_baseline_24h_ratio{set="default"}`) {
		t.Errorf("a set with no member past 24 h has a ratio:\n%s", out)
	}
}

// Without policy sets the runner keeps today's behaviour: per source 5
// per cycle at the fleet TTL and the source owner.
func TestPolicyEnrol_LegacyModeUnchanged(t *testing.T) {
	var cands []Candidate
	for i := range 6 {
		cands = append(cands, agentCand(fmt.Sprintf("c%d", i), "agent:x"))
	}
	st, members := newFakeStore(), &fakeMembers{}
	r, err := NewRunner(config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 50,
		TTLHours: 72}, []Binding{{Source: &fakeSource{name: "cluster",
		results: [][]Candidate{cands}}, Owner: "team-db", EnrolFrom: "main"}}, st, members)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.WithClock(func() time.Time { return enrolT0 })
	if rep := r.Cycle(context.Background()); rep.Enrolled != EnrolPerCycle {
		t.Fatalf("legacy enrolled %d, want %d", rep.Enrolled, EnrolPerCycle)
	}
	for ext, m := range members.byExternal() {
		if m.owner != "team-db" || !m.expires.Equal(enrolT0.Add(72*time.Hour)) {
			t.Fatalf("legacy %s enrolled as %+v", ext, m)
		}
	}
}
