//go:build integration

package discovery

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetpol-01b on a real server: the seeded policy sets, the cluster
// source's database comments and the discovery store's set columns.

// instantGrades is review-01's fake grader: a requested grade is graded
// at once (A on every dimension), as a member runtime would on day one.
type instantGrades struct {
	mu     sync.Mutex
	asked  []string
	grades map[string][]Grade
	clock  func() time.Time
}

func (g *instantGrades) RequestFirstGrade(_ context.Context, member string,
	_ store.DiscoveredRow) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, member)
	if g.grades == nil {
		g.grades = map[string][]Grade{}
	}
	g.grades[member] = append(g.grades[member], Grade{At: g.clock(),
		Letters: map[string]string{"safety": "A"}})
	return nil
}

func (g *instantGrades) Grades(_ context.Context, member string) ([]Grade, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.grades[member], nil
}

func setAgentEnrollment(t *testing.T, env liveEnv) {
	t.Helper()
	if _, err := env.pool.Exec(env.ctx, `UPDATE sage.policy_sets SET requirements =
		'{"ttl_hours": 24, "enrollment_deadline_hours": 24,
		  "enrollment": {"interval_minutes": 2, "owner": "creator"}}'::jsonb
		WHERE name = 'agent-created'`); err != nil {
		t.Fatalf("set agent-created enrollment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), `UPDATE sage.policy_sets
			SET requirements = '{}'::jsonb WHERE name = 'agent-created'`)
	})
}

func commentDatabase(t *testing.T, env liveEnv, name, comment string) {
	t.Helper()
	if _, err := env.pool.Exec(env.ctx, `COMMENT ON DATABASE `+quoteIdent(name)+
		` IS '`+strings.ReplaceAll(comment, "'", "''")+`'`); err != nil {
		t.Fatalf("comment %s: %v", name, err)
	}
}

func (e liveEnv) setsRunner(t *testing.T, members Members, now *time.Time,
	grades Grades) *Runner {
	t.Helper()
	return e.clusterRunner(t, members).WithClock(func() time.Time { return *now }).
		WithPolicySets(PolicySetsFrom(policy.NewStore(e.pool)), grades)
}

// Spec CHECK (CHECK-25): a fixture agent creates a database with the
// creator comment; one 2-minute cycle enrolls it under agent-created with
// its owner and TTL, and the fake grader grades it, with no human call.
// A person's database beside it follows default with the source owner.
func TestPolicyEnrol_FixtureAgentEnrolledAndGraded(t *testing.T) {
	env := requireLive(t)
	setAgentEnrollment(t, env)
	creatorDB := dbName(t, testdb.CreateDatabase(t, "agent_fix"))
	commentDatabase(t, env, creatorDB, "pg_sage: creator=agent:fixture tier=dev")
	personDB := dbName(t, testdb.CreateDatabase(t, "person_db"))
	now := time.Now().UTC().Truncate(time.Second)
	grades := &instantGrades{clock: func() time.Time { return now }}
	r := env.setsRunner(t, &liveMembers{env: env}, &now, grades)
	rep := r.Cycle(env.ctx)
	if len(rep.Errors) != 0 || rep.Sources["cluster"].Err != nil {
		t.Fatalf("cycle: %+v", rep)
	}
	row := findRow(t, env, creatorDB)
	if row.Status != store.DiscoveryEnrolled || row.SetName != "agent-created" ||
		row.Creator != "agent:fixture" || row.Tags["tier"] != "dev" || row.DatabaseID == nil {
		t.Fatalf("agent row = %+v", row)
	}
	if row.EnrolledAt == nil || row.FirstGradedAt == nil || row.BaselineAt == nil ||
		row.GradeRequestedAt == nil {
		t.Fatalf("agent milestones = enrolled %v requested %v graded %v baseline %v",
			row.EnrolledAt, row.GradeRequestedAt, row.FirstGradedAt, row.BaselineAt)
	}
	var owner, trust string
	var expires time.Time
	if err := env.pool.QueryRow(env.ctx, `SELECT owner, trust_level, expires_at
		FROM sage.databases WHERE id = $1`, *row.DatabaseID).Scan(&owner, &trust,
		&expires); err != nil {
		t.Fatalf("read member: %v", err)
	}
	if owner != "agent:fixture" || trust != "observation" ||
		!expires.Equal(row.EnrolledAt.Add(24*time.Hour)) {
		t.Fatalf("member owner %q trust %q expires %v (enrolled %v)", owner, trust,
			expires, row.EnrolledAt)
	}
	assertResolvesTo(t, env, int64(*row.DatabaseID), "agent-created", "agent:fixture")
	person := findRow(t, env, personDB)
	if person.SetName != policy.DefaultSetName || person.Status != store.DiscoveryEnrolled {
		t.Fatalf("person row = %+v", person)
	}
	assertMetricsHaveSets(t, r, "agent-created", policy.DefaultSetName)
}

// assertResolvesTo checks that the gate's policy resolution puts member
// in set, with creator, once the sets have documents (the sidecar gives
// every set its first document when it starts).
func assertResolvesTo(t *testing.T, env liveEnv, member int64, set, creator string) {
	t.Helper()
	ps := policy.NewStore(env.pool)
	if _, err := ps.Bootstrap(env.ctx, policy.Scope{DatabaseID: &member}, "unattended",
		"bootstrap"); err != nil {
		t.Fatalf("bootstrap set documents: %v", err)
	}
	res, err := ps.Resolve(env.ctx, member)
	if err != nil || res.Set != set || res.Member.Creator != creator {
		t.Fatalf("policy resolution = %+v %v, want %s", res, err, set)
	}
}

func assertMetricsHaveSets(t *testing.T, r *Runner, sets ...string) {
	t.Helper()
	var b strings.Builder
	r.WriteMetrics(&b)
	for _, set := range sets {
		for _, want := range []string{
			`pg_sage_fleet_enroll_seconds{set="` + set + `",quantile="0.5"}`,
			`pg_sage_fleet_enroll_seconds{set="` + set + `",quantile="0.95"}`,
			`pg_sage_fleet_baseline_24h_ratio{set="` + set + `"}`,
		} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("metrics lack %s:\n%s", want, b.String())
			}
		}
	}
}

// Invalid input: an empty creator in the comment is ignored with a reason
// and the database follows default, not agent-created.
func TestPolicyEnrol_EmptyCreatorCommentFollowsDefault(t *testing.T) {
	env := requireLive(t)
	setAgentEnrollment(t, env)
	name := dbName(t, testdb.CreateDatabase(t, "empty_creator"))
	commentDatabase(t, env, name, "pg_sage: creator= owner=team-x")
	now := time.Now().UTC()
	r := env.setsRunner(t, &liveMembers{env: env}, &now,
		&instantGrades{clock: func() time.Time { return now }})
	rep := r.Cycle(env.ctx)
	row := findRow(t, env, name)
	if row.SetName != policy.DefaultSetName || row.Creator != "" ||
		row.Tags["owner"] != "team-x" {
		t.Fatalf("row = %+v", row)
	}
	if !strings.Contains(strings.Join(rep.Ignored, ";"), "creator") {
		t.Fatalf("no reason logged: %v", rep.Ignored)
	}
}

// Boundary: default's per_cycle 5 with 6 new databases enrolls 5, and the
// sixth on default's next cycle (10 minutes later).
func TestPolicyEnrol_PerCycleOnTheRealStore(t *testing.T) {
	env := requireLive(t)
	var names []string
	for range 6 {
		names = append(names, dbName(t, testdb.CreateDatabase(t, "per_cycle")))
	}
	now := time.Now().UTC()
	r := env.setsRunner(t, &liveMembers{env: env}, &now, nil)
	rep := r.Cycle(env.ctx)
	// Earlier tests' databases may still be on the cluster: count them.
	total := rep.Sources["cluster"].Listed
	if total < 6 || total > 10 || rep.PerSet[policy.DefaultSetName] != 5 {
		t.Fatalf("first cycle: %+v, want 5 of %d enrolled", rep, total)
	}
	now = now.Add(2 * time.Minute)
	if rep := r.Cycle(env.ctx); rep.Enrolled != 0 {
		t.Fatalf("cycle before default's interval: %+v", rep)
	}
	now = now.Add(8 * time.Minute)
	if rep := r.Cycle(env.ctx); rep.PerSet[policy.DefaultSetName] != total-5 {
		t.Fatalf("second cycle: %+v, want the other %d", rep, total-5)
	}
	for _, n := range names {
		if row := findRow(t, env, n); row.Status != store.DiscoveryEnrolled {
			t.Fatalf("%s is %s", n, row.Status)
		}
	}
}

// Concurrency: a leader and a follower cycling at once never enroll one
// candidate twice (the Claim advisory lock), in set mode too.
func TestPolicyEnrol_LeaderAndFollowerNoDuplicates(t *testing.T) {
	env := requireLive(t)
	setAgentEnrollment(t, env)
	var names []string
	for range 3 {
		n := dbName(t, testdb.CreateDatabase(t, "dup_set"))
		commentDatabase(t, env, n, "pg_sage: creator=agent:dup")
		names = append(names, n)
	}
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	r1 := env.setsRunner(t, &liveMembers{env: env}, &now, &instantGrades{clock: clock})
	r2 := env.setsRunner(t, &liveMembers{env: env}, &now, &instantGrades{clock: clock})
	var wg sync.WaitGroup
	for _, r := range []*Runner{r1, r2, r1, r2} {
		wg.Add(1)
		go func(r *Runner) { defer wg.Done(); r.Cycle(env.ctx) }(r)
	}
	wg.Wait()
	for _, n := range names {
		var rows, members int
		_ = env.pool.QueryRow(env.ctx, `SELECT count(*) FROM sage.discovered_databases
			WHERE dbname = $1`, n).Scan(&rows)
		_ = env.pool.QueryRow(env.ctx, `SELECT count(*) FROM sage.databases
			WHERE database_name = $1`, n).Scan(&members)
		if rows != 1 || members != 1 {
			t.Fatalf("%s: rows=%d members=%d, want 1 and 1", n, rows, members)
		}
	}
}

// Bootstrap twice is clean and leaves the set columns in place.
func TestBootstrapTwice_DiscoveredAttributes(t *testing.T) {
	env := requireLive(t)
	if err := schema.Bootstrap(env.ctx, env.pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	var cols int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'discovered_databases'
		  AND column_name IN ('tags', 'creator', 'created_at', 'set_name', 'enrolled_at',
		      'grade_requested_at', 'first_graded_at', 'baseline_at')`).Scan(&cols); err != nil {
		t.Fatalf("columns: %v", err)
	}
	if cols != 8 {
		t.Fatalf("discovered_databases has %d of the 8 fleetpol-01b columns", cols)
	}
}
