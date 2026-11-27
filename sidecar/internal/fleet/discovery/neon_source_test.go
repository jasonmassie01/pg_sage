package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// fakeNeon is a scripted Neon listing.
type fakeNeon struct {
	mu        sync.Mutex
	projects  []clone.NeonProject
	branches  map[string][]clone.NeonListedBranch
	endpoints map[string][]clone.NeonListedEndpoint
	errs      map[string]error // "projects", "branches:<p>", "endpoints:<p>"
	calls     []string
}

func (f *fakeNeon) call(what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what)
	return f.errs[what]
}

func (f *fakeNeon) ListProjects(context.Context) ([]clone.NeonProject, error) {
	if err := f.call("projects"); err != nil {
		return nil, err
	}
	return f.projects, nil
}

func (f *fakeNeon) ListBranches(_ context.Context, p string) ([]clone.NeonListedBranch, error) {
	if err := f.call("branches:" + p); err != nil {
		return nil, err
	}
	return f.branches[p], nil
}

func (f *fakeNeon) ListEndpoints(_ context.Context,
	p string) ([]clone.NeonListedEndpoint, error) {
	if err := f.call("endpoints:" + p); err != nil {
		return nil, err
	}
	return f.endpoints[p], nil
}

var neonT0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func neonFixture() *fakeNeon {
	return &fakeNeon{
		projects: []clone.NeonProject{{ID: "p-one"}},
		branches: map[string][]clone.NeonListedBranch{"p-one": {
			{ID: "br-main", Name: "main", ProjectID: "p-one", Default: true,
				CreatedAt: neonT0.Add(-48 * time.Hour), CreationSource: "console",
				CreatedBy: "Ada"},
			{ID: "br-agent", Name: "agent-1", ProjectID: "p-one", CreatedAt: neonT0,
				CreationSource: "api"},
			{ID: "br-rh", Name: "sage-rh-0a1b2c3d", ProjectID: "p-one", CreatedAt: neonT0},
			{ID: "br-noep", Name: "cold", ProjectID: "p-one", CreatedAt: neonT0},
		}},
		endpoints: map[string][]clone.NeonListedEndpoint{"p-one": {
			{ID: "ep-ro", Host: "ep-ro.neon.tech", BranchID: "br-main", Type: "read_only"},
			{ID: "ep-main", Host: "EP-Main.neon.tech", BranchID: "br-main",
				Type: "read_write"},
			{ID: "ep-agent", Host: "ep-agent.neon.tech", BranchID: "br-agent",
				Type: "read_write"},
			{ID: "ep-rh", Host: "ep-rh.neon.tech", BranchID: "br-rh", Type: "read_write"},
		}},
	}
}

// Happy path: one candidate per branch with an endpoint, the read-write
// endpoint's host, creator and creation time from the API; pg_sage's
// rehearsal branches are skipped (market-06b).
func TestNeonSource_ListsBranchesWithCreatorAndCreation(t *testing.T) {
	api := neonFixture()
	got, err := NewNeonSource("neon", "", "", api, CycleCap).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want main and agent-1", got)
	}
	main, agent := got[0], got[1]
	if main.ExternalID != "br-main" || main.Host != "ep-main.neon.tech" || main.Port != 5432 ||
		main.DBName != "neondb" || main.Creator != "person:Ada" ||
		!main.CreatedAt.Equal(neonT0.Add(-48*time.Hour)) {
		t.Fatalf("main = %+v", main)
	}
	if agent.ExternalID != "br-agent" || agent.Creator != "agent:api" ||
		!agent.CreatedAt.Equal(neonT0) || agent.Tags["neon_project"] != "p-one" ||
		agent.Tags["neon_branch"] != "agent-1" {
		t.Fatalf("agent = %+v", agent)
	}
	for _, c := range got {
		if strings.HasPrefix(c.Tags["neon_branch"], "sage-") {
			t.Fatalf("rehearsal branch listed: %+v", c)
		}
	}
}

// A configured project is listed alone (no project listing) and the
// configured database name is used.
func TestNeonSource_ConfiguredProjectAndDatabase(t *testing.T) {
	api := neonFixture()
	got, err := NewNeonSource("neon", "p-one", "app", api, CycleCap).List(context.Background())
	if err != nil || len(got) != 2 || got[0].DBName != "app" {
		t.Fatalf("List = %+v %v", got, err)
	}
	for _, c := range api.calls {
		if c == "projects" {
			t.Fatal("a configured project still listed every project")
		}
	}
}

// Nil/empty: no projects record nothing; a project with no branches, or
// a branch with an unknown creation source, adds nothing odd.
func TestNeonSource_EmptyListings(t *testing.T) {
	got, err := NewNeonSource("neon", "", "", &fakeNeon{}, CycleCap).List(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("no projects: %+v %v", got, err)
	}
	api := &fakeNeon{projects: []clone.NeonProject{{ID: "p"}},
		branches: map[string][]clone.NeonListedBranch{"p": {{ID: "br-x", Name: "x"}}},
		endpoints: map[string][]clone.NeonListedEndpoint{"p": {{ID: "e", Host: "e.neon.tech",
			BranchID: "br-x", Type: "read_write"}}}}
	got, err = NewNeonSource("neon", "", "", api, CycleCap).List(context.Background())
	if err != nil || len(got) != 1 || got[0].Creator != "" || !got[0].CreatedAt.IsZero() {
		t.Fatalf("bare branch: %+v %v", got, err)
	}
}

// Boundary: the cap bounds the candidates and the projects walked.
func TestNeonSource_CapBoundsTheListing(t *testing.T) {
	api := &fakeNeon{branches: map[string][]clone.NeonListedBranch{},
		endpoints: map[string][]clone.NeonListedEndpoint{}}
	for i := range 5 {
		p := fmt.Sprintf("p-%d", i)
		api.projects = append(api.projects, clone.NeonProject{ID: p})
		for j := range 3 {
			b := fmt.Sprintf("br-%d-%d", i, j)
			api.branches[p] = append(api.branches[p], clone.NeonListedBranch{ID: b, Name: b})
			api.endpoints[p] = append(api.endpoints[p], clone.NeonListedEndpoint{ID: "e" + b,
				Host: b + ".neon.tech", BranchID: b, Type: "read_write"})
		}
	}
	got, err := NewNeonSource("neon", "", "", api, 4).List(context.Background())
	if err != nil || len(got) != 4 {
		t.Fatalf("cap 4: %d %v", len(got), err)
	}
	walked := 0
	for _, c := range api.calls {
		if strings.HasPrefix(c, "branches:") {
			walked++
		}
	}
	if walked != 2 {
		t.Fatalf("projects walked = %d, want 2 (stop at the cap)", walked)
	}
}

// Error propagation: a Neon 429 marks the source run failed with the
// status, the source backs off on the next cycle, and the other sources
// still run.
func TestNeonSource_ThrottledRunFailsBacksOffAndOthersRun(t *testing.T) {
	throttled := fmt.Errorf("neon list branches: %w: slow down",
		&clone.ProviderStatusError{Op: "neon list branches", StatusCode: 429})
	api := neonFixture()
	api.errs = map[string]error{"branches:p-one": throttled}
	neon := NewNeonSource("neon", "", "", api, CycleCap)
	other := &fakeSource{name: "cluster", results: [][]Candidate{{cand("a")}}}
	st := newFakeStore()
	r, err := NewRunner(config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 10,
		TTLHours: 72}, []Binding{{Source: neon, Owner: "team"},
		{Source: other, Owner: "team"}}, st, &fakeMembers{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	now := neonT0
	r.WithClock(func() time.Time { return now })
	rep := r.Cycle(context.Background())
	nr := rep.Sources["neon"]
	var status *clone.ProviderStatusError
	if !errors.As(nr.Err, &status) || status.StatusCode != 429 {
		t.Fatalf("neon report = %+v, want the 429", nr)
	}
	if runs := st.runs["neon"]; len(runs) != 1 || runs[0] == nil ||
		!strings.Contains(runs[0].Error(), "429") {
		t.Fatalf("recorded neon runs = %v, want one failure naming 429", runs)
	}
	if rep.Sources["cluster"].Err != nil || rep.Sources["cluster"].Listed != 1 {
		t.Fatalf("cluster report = %+v, want listed", rep.Sources["cluster"])
	}
	before := len(api.calls)
	now = now.Add(2 * time.Minute)
	rep = r.Cycle(context.Background())
	if !rep.Sources["neon"].BackingOff || len(api.calls) != before {
		t.Fatalf("second cycle: neon %+v and %d new calls, want a back-off and none",
			rep.Sources["neon"], len(api.calls)-before)
	}
	if other.callCount() != 2 {
		t.Fatalf("cluster listed %d times, want 2", other.callCount())
	}
	now = now.Add(time.Hour)
	api.errs = nil
	if rep = r.Cycle(context.Background()); rep.Sources["neon"].Err != nil ||
		rep.Sources["neon"].BackingOff || rep.Sources["neon"].Listed != 2 {
		t.Fatalf("after the back-off: %+v", rep.Sources["neon"])
	}
}

// A non-throttled error (a bad key) fails the run but does not back off.
func TestNeonSource_OtherErrorsDoNotBackOff(t *testing.T) {
	api := neonFixture()
	api.errs = map[string]error{"projects": errors.New("neon list projects: 401")}
	r, err := NewRunner(config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 10,
		TTLHours: 72}, []Binding{{Source: NewNeonSource("neon", "", "", api, CycleCap),
		Owner: "team"}}, newFakeStore(), &fakeMembers{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if rep := r.Cycle(context.Background()); rep.Sources["neon"].Err == nil {
		t.Fatal("401 not reported")
	}
	if rep := r.Cycle(context.Background()); rep.Sources["neon"].BackingOff {
		t.Fatal("a 401 backed off; only 429 and 5xx should")
	}
}

// Config: a neon source needs no project (every project the key lists)
// and builds through Bindings with the Neon client.
func TestBindings_BuildsANeonSource(t *testing.T) {
	cfg := config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 5, TTLHours: 24,
		Sources: []config.DiscoverySourceConfig{{Name: "neon", Type: "neon", Owner: "team",
			Database: "app"}}}
	var asked bool
	bs, err := Bindings(cfg, SourceDeps{Neon: func() (NeonAPI, error) {
		asked = true
		return neonFixture(), nil
	}})
	if err != nil || len(bs) != 1 || !asked || bs[0].EnrolFrom != "" {
		t.Fatalf("Bindings = %+v %v (client asked %v)", bs, err, asked)
	}
	if _, ok := bs[0].Source.(*NeonSource); !ok {
		t.Fatalf("source = %T, want *NeonSource", bs[0].Source)
	}
	_, err = Bindings(cfg, SourceDeps{Neon: func() (NeonAPI, error) {
		return nil, errors.New("clone.neon.api_key is empty")
	}})
	if err == nil || !strings.Contains(err.Error(), "neon") {
		t.Fatalf("missing key: %v", err)
	}
}
