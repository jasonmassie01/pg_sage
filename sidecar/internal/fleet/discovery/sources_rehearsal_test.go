package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/databricks/databricks-sdk-go/listing"
	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// market-06b: pg_sage's own sage-rh-* rehearsal branches are never listed.
// No concurrent access tests: List is called by one discovery loop per source.
// No state transition tests: the filter is stateless.
// No integration tests: there is no local Lakebase (CHECK-37 is an owner check).

// branchLakebase serves the given branch names, and for each branch whose
// endpoints are listed one endpoint with uid "uid-<branch id>".
func branchLakebase(names ...string) *fakeLakebase {
	return &fakeLakebase{
		branches: func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
			items := make([]postgres.Branch, 0, len(names))
			for _, n := range names {
				items = append(items, postgres.Branch{Name: n})
			}
			return &sliceIter[postgres.Branch]{items: items}
		},
		endpoints: func(r postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
			id := r.Parent[strings.LastIndex(r.Parent, "/")+1:]
			return &sliceIter[postgres.Endpoint]{items: []postgres.Endpoint{
				endpoint(r.Parent+"/endpoints/rw", "uid-"+id, id+".db.example")}}
		},
	}
}

// endpointParents is every ListEndpoints parent the fake saw, in order.
func endpointParents(api *fakeLakebase) []string {
	var out []string
	for _, p := range api.parents {
		if strings.Contains(p, "/branches/") || !strings.HasPrefix(p, "projects/") {
			out = append(out, p)
		}
	}
	return out
}

func candidateIDs(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ExternalID)
	}
	return out
}

func TestLakebase_SkipsRehearsalBranches(t *testing.T) {
	api := branchLakebase("projects/p1/branches/main",
		"projects/p1/branches/sage-rh-0a1b2c3d", "projects/p1/branches/dev")
	got, err := NewLakebaseSource("lb", "p1", api, 50).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := candidateIDs(got)
	if strings.Join(ids, ",") != "uid-main,uid-dev" {
		t.Fatalf("candidates = %v, want uid-main,uid-dev", ids)
	}
	if got[1].Host != "dev.db.example" || got[1].DBName != "databricks_postgres" ||
		got[1].Port != 5432 {
		t.Fatalf("dev candidate = %+v", got[1])
	}
	if api.endpointCalls != 2 {
		t.Fatalf("ListEndpoints calls = %d, want 2 (never the rehearsal branch)",
			api.endpointCalls)
	}
	for _, p := range endpointParents(api) {
		if strings.Contains(p, "sage-rh-") {
			t.Fatalf("ListEndpoints called for rehearsal branch %q", p)
		}
	}
}

// Invalid input: only a branch id that starts with the prefix is skipped; a
// bare "sage-rh", an id that contains it later, or the prefix in the project
// name is an ordinary branch.
func TestLakebase_RehearsalPrefixMatchesBranchIDOnly(t *testing.T) {
	api := branchLakebase("projects/p1/branches/sage-rh",
		"projects/p1/branches/my-sage-rh-1", "projects/sage-rh-p/branches/main",
		"projects/p1/branches/sage-rh0a1b2c3d")
	got, err := NewLakebaseSource("lb", "p1", api, 50).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := "uid-sage-rh,uid-my-sage-rh-1,uid-main,uid-sage-rh0a1b2c3d"
	if ids := strings.Join(candidateIDs(got), ","); ids != want {
		t.Fatalf("candidates = %s, want %s", ids, want)
	}
	if api.endpointCalls != 4 {
		t.Fatalf("ListEndpoints calls = %d, want 4", api.endpointCalls)
	}
}

// Invalid input: a branch name without the projects/ path is judged by the
// whole name, which is then its id.
func TestLakebase_SkipsBareRehearsalBranchName(t *testing.T) {
	api := branchLakebase("sage-rh-0a1b2c3d", "main")
	got, err := NewLakebaseSource("lb", "p1", api, 50).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := strings.Join(candidateIDs(got), ","); ids != "uid-main" {
		t.Fatalf("candidates = %s, want uid-main", ids)
	}
	if api.endpointCalls != 1 {
		t.Fatalf("ListEndpoints calls = %d, want 1", api.endpointCalls)
	}
}

// Nil/empty: a project with no branches lists nothing and is not an error.
func TestLakebase_NoBranchesIsEmpty(t *testing.T) {
	api := branchLakebase()
	got, err := NewLakebaseSource("lb", "p1", api, 50).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 || api.endpointCalls != 0 || api.branchCalls != 1 {
		t.Fatalf("got %v, endpointCalls %d, branchCalls %d; want empty, 0, 1",
			got, api.endpointCalls, api.branchCalls)
	}
}

// Nil/empty: a project whose only branches are rehearsals lists nothing.
func TestLakebase_OnlyRehearsalBranchesIsEmpty(t *testing.T) {
	api := branchLakebase("projects/p1/branches/sage-rh-00000000",
		"projects/p1/branches/sage-rh-ffffffff")
	got, err := NewLakebaseSource("lb", "p1", api, 50).List(context.Background())
	if err != nil || len(got) != 0 || api.endpointCalls != 0 {
		t.Fatalf("got %v, %v, endpointCalls %d; want empty, nil, 0", got, err,
			api.endpointCalls)
	}
}

// Error propagation: a ListBranches error keeps its cause for errors.Is and
// names the project.
func TestLakebase_BranchErrorKeepsCause(t *testing.T) {
	cause := errors.New("RESOURCE_EXHAUSTED")
	api := &fakeLakebase{
		branches: func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
			return &loopIter[postgres.Branch]{err: cause}
		},
	}
	got, err := NewLakebaseSource("lb", "p1", api, 5).List(context.Background())
	if !errors.Is(err, cause) || got != nil {
		t.Fatalf("List = %v, %v; want nil and the ListBranches cause", got, err)
	}
	if !strings.Contains(err.Error(), "projects/p1") ||
		!strings.Contains(err.Error(), "lakebase source lb") {
		t.Fatalf("err = %v, want the source and project named", err)
	}
	if api.endpointCalls != 0 {
		t.Fatalf("ListEndpoints calls = %d after a branch error", api.endpointCalls)
	}
}

// Error propagation: an endpoint error on the branch after a skipped
// rehearsal still surfaces with that branch's name.
func TestLakebase_EndpointErrorAfterSkippedRehearsal(t *testing.T) {
	cause := errors.New("PERMISSION_DENIED")
	api := branchLakebase("projects/p1/branches/sage-rh-0a1b2c3d", "projects/p1/branches/dev")
	api.endpoints = func(postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
		return &loopIter[postgres.Endpoint]{err: cause}
	}
	got, err := NewLakebaseSource("lb", "p1", api, 5).List(context.Background())
	if !errors.Is(err, cause) || got != nil {
		t.Fatalf("List = %v, %v; want nil and the ListEndpoints cause", got, err)
	}
	if !strings.Contains(err.Error(), "projects/p1/branches/dev") {
		t.Fatalf("err = %v, want the dev branch named", err)
	}
}

// Boundary: a skipped branch still counts toward the cap, so the walk stays
// bounded by branches seen. With cap 2 and two rehearsal branches first,
// main is never reached.
func TestLakebase_SkippedBranchesCountTowardCap(t *testing.T) {
	api := branchLakebase("projects/p1/branches/sage-rh-1",
		"projects/p1/branches/sage-rh-2", "projects/p1/branches/main")
	got, err := NewLakebaseSource("lb", "p1", api, 2).List(context.Background())
	if err != nil || len(got) != 0 || api.endpointCalls != 0 {
		t.Fatalf("got %v, %v, endpointCalls %d; want no candidate within the cap",
			got, err, api.endpointCalls)
	}
	// One more slot reaches main.
	api = branchLakebase("projects/p1/branches/sage-rh-1",
		"projects/p1/branches/sage-rh-2", "projects/p1/branches/main")
	got, err = NewLakebaseSource("lb", "p1", api, 3).List(context.Background())
	if err != nil || strings.Join(candidateIDs(got), ",") != "uid-main" {
		t.Fatalf("cap 3: got %v, %v; want uid-main", candidateIDs(got), err)
	}
}

// Boundary: an endless stream of rehearsal branches stops at the cap
// instead of spinning, without listing any endpoints.
func TestLakebase_RehearsalBranchLoopStopsAtCap(t *testing.T) {
	var branches *loopIter[postgres.Branch]
	api := &fakeLakebase{
		branches: func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
			branches = &loopIter[postgres.Branch]{next: func(int) postgres.Branch {
				return postgres.Branch{Name: "projects/p1/branches/sage-rh-0a1b2c3d"}
			}}
			return branches
		},
		endpoints: func(postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
			return &sliceIter[postgres.Endpoint]{}
		},
	}
	got, err := NewLakebaseSource("lb", "p1", api, 7).List(context.Background())
	if err != nil || len(got) != 0 || api.endpointCalls != 0 {
		t.Fatalf("got %v, %v, endpointCalls %d", got, err, api.endpointCalls)
	}
	if branches.n != 7 {
		t.Fatalf("walked %d branches, want the cap 7", branches.n)
	}
}
