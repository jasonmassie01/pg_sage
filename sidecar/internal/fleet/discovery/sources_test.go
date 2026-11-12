package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/databricks/databricks-sdk-go/listing"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// loopIter never ends: a server whose page token always points at another
// page. next builds the i-th item.
type loopIter[T any] struct {
	n    int
	next func(i int) T
	err  error
}

func (l *loopIter[T]) HasNext(context.Context) bool { return true }

func (l *loopIter[T]) Next(context.Context) (T, error) {
	var zero T
	if l.err != nil {
		return zero, l.err
	}
	l.n++
	return l.next(l.n), nil
}

type sliceIter[T any] struct{ items []T }

func (s *sliceIter[T]) HasNext(context.Context) bool { return len(s.items) > 0 }

func (s *sliceIter[T]) Next(context.Context) (T, error) {
	it := s.items[0]
	s.items = s.items[1:]
	return it, nil
}

type fakeLakebase struct {
	branches      func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch]
	endpoints     func(postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint]
	branchCalls   int
	endpointCalls int
	parents       []string
}

func (f *fakeLakebase) ListBranches(_ context.Context,
	r postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
	f.branchCalls++
	f.parents = append(f.parents, r.Parent)
	return f.branches(r)
}

func (f *fakeLakebase) ListEndpoints(_ context.Context,
	r postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
	f.endpointCalls++
	f.parents = append(f.parents, r.Parent)
	return f.endpoints(r)
}

func endpoint(name, uid, host string) postgres.Endpoint {
	return postgres.Endpoint{Name: name, Uid: uid,
		Status: &postgres.EndpointStatus{Hosts: &postgres.EndpointHosts{Host: host}}}
}

func TestLakebase_ListsBranchEndpoints(t *testing.T) {
	api := &fakeLakebase{
		branches: func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
			return &sliceIter[postgres.Branch]{items: []postgres.Branch{
				{Name: "projects/p1/branches/main"}, {Name: "projects/p1/branches/dev"}}}
		},
		endpoints: func(r postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
			if strings.HasSuffix(r.Parent, "main") {
				return &sliceIter[postgres.Endpoint]{items: []postgres.Endpoint{
					endpoint(r.Parent+"/endpoints/rw", "uid-main", "main.db.example")}}
			}
			return &sliceIter[postgres.Endpoint]{items: []postgres.Endpoint{
				endpoint(r.Parent+"/endpoints/rw", "uid-dev", "dev.db.example"),
				{Name: r.Parent + "/endpoints/nohost", Uid: "uid-x"}}}
		},
	}
	src := NewLakebaseSource("lb", "p1", api, 50)
	got, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ExternalID != "uid-main" || got[1].Host != "dev.db.example" {
		t.Fatalf("candidates = %+v", got)
	}
	if got[0].Port != 5432 || got[0].DBName != "databricks_postgres" {
		t.Fatalf("candidate defaults = %+v", got[0])
	}
	if api.parents[0] != "projects/p1" {
		t.Fatalf("branch parent = %q", api.parents[0])
	}
	if src.Name() != "lb" {
		t.Fatalf("Name() = %q", src.Name())
	}
}

// Invalid input: a page-token loop (an iterator that never ends) stops at
// the cap instead of spinning.
func TestLakebase_PageTokenLoopStopsAtCap(t *testing.T) {
	api := &fakeLakebase{
		branches: func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
			return &loopIter[postgres.Branch]{next: func(i int) postgres.Branch {
				return postgres.Branch{Name: "projects/p1/branches/b"}
			}}
		},
		endpoints: func(r postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
			return &loopIter[postgres.Endpoint]{next: func(i int) postgres.Endpoint {
				return endpoint(r.Parent+"/endpoints/e", "uid", "h")
			}}
		},
	}
	got, err := NewLakebaseSource("lb", "p1", api, 7).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("candidates = %d, want the cap 7", len(got))
	}
	// Branches without endpoints loop too: the branch walk is capped.
	api.endpoints = func(postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
		return &sliceIter[postgres.Endpoint]{}
	}
	api.endpointCalls = 0
	got, err = NewLakebaseSource("lb", "p1", api, 7).List(context.Background())
	if err != nil || len(got) != 0 || api.endpointCalls != 7 {
		t.Fatalf("empty-branch loop: got=%d err=%v endpointCalls=%d", len(got), err,
			api.endpointCalls)
	}
}

func TestLakebase_IteratorErrorPropagates(t *testing.T) {
	api := &fakeLakebase{
		branches: func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
			return &loopIter[postgres.Branch]{err: errors.New("PERMISSION_DENIED")}
		},
	}
	_, err := NewLakebaseSource("lb", "p1", api, 5).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "PERMISSION_DENIED") ||
		!strings.Contains(err.Error(), "projects/p1") {
		t.Fatalf("err = %v, want the cause and the project", err)
	}
}

type fakeRDS struct {
	pages  []*rds.DescribeDBInstancesOutput
	err    error
	inputs []*rds.DescribeDBInstancesInput
}

func (f *fakeRDS) DescribeDBInstances(_ context.Context, in *rds.DescribeDBInstancesInput,
	_ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	i := len(f.inputs) - 1
	if i >= len(f.pages) {
		i = len(f.pages) - 1
	}
	return f.pages[i], nil
}

func rdsInstance(id, team string) rdstypes.DBInstance {
	return rdstypes.DBInstance{DBInstanceIdentifier: aws.String(id),
		DbiResourceId: aws.String("db-" + id), DBName: aws.String("app"),
		Endpoint: &rdstypes.Endpoint{Address: aws.String(id + ".rds.example"),
			Port: aws.Int32(5433)},
		TagList: []rdstypes.Tag{{Key: aws.String("team"), Value: aws.String(team)}}}
}

func TestRDS_TagFilterAndPaging(t *testing.T) {
	api := &fakeRDS{pages: []*rds.DescribeDBInstancesOutput{
		{DBInstances: []rdstypes.DBInstance{rdsInstance("a", "db"), rdsInstance("b", "web")},
			Marker: aws.String("m1")},
		{DBInstances: []rdstypes.DBInstance{rdsInstance("c", "db"),
			{DBInstanceIdentifier: aws.String("noendpoint"), DbiResourceId: aws.String("x"),
				TagList: []rdstypes.Tag{{Key: aws.String("team"), Value: aws.String("db")}}}}},
	}}
	got, err := NewRDSSource("aws", "team", "db", api, 100).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ExternalID != "db-a" || got[1].ExternalID != "db-c" {
		t.Fatalf("candidates = %+v", got)
	}
	if got[0].Host != "a.rds.example" || got[0].Port != 5433 || got[0].DBName != "app" {
		t.Fatalf("candidate fields = %+v", got[0])
	}
	if len(api.inputs) != 2 || aws.ToString(api.inputs[1].Marker) != "m1" {
		t.Fatalf("paging inputs = %d, marker %q", len(api.inputs),
			aws.ToString(api.inputs[len(api.inputs)-1].Marker))
	}
	if f := api.inputs[0].Filters; len(f) != 1 || aws.ToString(f[0].Name) != "engine" {
		t.Fatalf("engine filter missing: %+v", f)
	}
}

func TestRDS_RepeatedMarkerStops(t *testing.T) {
	api := &fakeRDS{pages: []*rds.DescribeDBInstancesOutput{
		{DBInstances: []rdstypes.DBInstance{rdsInstance("a", "db")}, Marker: aws.String("same")},
	}}
	got, err := NewRDSSource("aws", "team", "", api, 100).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(api.inputs) > 3 {
		t.Fatalf("a repeated marker was followed %d times", len(api.inputs))
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want one (empty tag value matches any value)", len(got))
	}
}

func TestRDS_ErrorPropagates(t *testing.T) {
	api := &fakeRDS{err: errors.New("api error AccessDenied: StatusCode: 403")}
	_, err := NewRDSSource("aws", "team", "db", api, 100).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") ||
		!strings.Contains(err.Error(), "DescribeDBInstances") {
		t.Fatalf("err = %v", err)
	}
}

type failingBeginner struct{ err error }

func (f failingBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, f.err
}

func TestCluster_ErrorPropagation(t *testing.T) {
	src := NewClusterSource("c", "h", 5432, func() (Beginner, error) {
		return failingBeginner{err: errors.New("connection refused")}, nil
	}, 10)
	_, err := src.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
	src = NewClusterSource("c", "h", 5432, func() (Beginner, error) {
		return nil, errors.New(`member "main" is not connected`)
	}, 10)
	if _, err := src.List(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Fatalf("missing pool err = %v", err)
	}
}

func TestBindings_FromConfig(t *testing.T) {
	cfg := config.FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 1, TTLHours: 1,
		Sources: []config.DiscoverySourceConfig{
			{Name: "c", Type: "cluster", Owner: "o", Member: "main", ReuseCredentials: true},
			{Name: "c2", Type: "cluster", Owner: "o", Member: "main"},
			{Name: "lb", Type: "lakebase", Owner: "o", Project: "p"},
			{Name: "aws", Type: "rds", Owner: "o", Region: "us-east-1", TagKey: "k"},
		}}
	deps := SourceDeps{
		ClusterPool: func(string) (Beginner, error) { return nil, errors.New("unused") },
		Member:      func(string) (string, int, error) { return "h", 5432, nil },
		Lakebase:    func() (LakebaseAPI, error) { return &fakeLakebase{}, nil },
		RDS:         func(string) (RDSAPI, error) { return &fakeRDS{}, nil },
	}
	b, err := Bindings(cfg, deps)
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	if len(b) != 4 || b[0].EnrolFrom != "main" || b[1].EnrolFrom != "" ||
		b[2].EnrolFrom != "" || b[3].Owner != "o" || b[2].Source.Name() != "lb" {
		t.Fatalf("bindings = %+v", b)
	}
	cfg.Sources[0].Owner = ""
	if _, err := Bindings(cfg, deps); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("invalid config accepted: %v", err)
	}
	cfg.Sources[0].Owner = "o"
	deps.Lakebase = func() (LakebaseAPI, error) { return nil, errors.New("no workspace") }
	if _, err := Bindings(cfg, deps); err == nil || !strings.Contains(err.Error(), "lb") {
		t.Fatalf("lakebase client error not attributed: %v", err)
	}
}

// Nil/empty: no sources, no bindings, no error.
func TestBindings_Empty(t *testing.T) {
	b, err := Bindings(config.FleetDiscoveryConfig{TTLHours: 1}, SourceDeps{})
	if err != nil || len(b) != 0 {
		t.Fatalf("empty config: %v %v", b, err)
	}
}

func TestMemberName_UniqueBoundedAndSafe(t *testing.T) {
	long := store.DiscoveredRow{ID: 12345, Source: "cluster",
		DBName: strings.Repeat("x", 80)}
	if got := MemberName(long); len(got) != 63 || !strings.HasSuffix(got, "-12345") {
		t.Fatalf("long name = %q (%d)", got, len(got))
	}
	got := MemberName(store.DiscoveredRow{ID: 7, Source: "lb", DBName: "my db;drop"})
	if got != "lb-my_db_drop-7" {
		t.Fatalf("unsafe characters kept: %q", got)
	}
}

// Invalid input: an endless page of endpoints without a host adds no
// candidate, and the walk still stops at the cap.
func TestLakebase_HostlessEndpointLoopStopsAtCap(t *testing.T) {
	api := &fakeLakebase{
		branches: func(postgres.ListBranchesRequest) listing.Iterator[postgres.Branch] {
			return &sliceIter[postgres.Branch]{items: []postgres.Branch{
				{Name: "projects/p1/branches/main"}}}
		},
	}
	var eps *loopIter[postgres.Endpoint]
	api.endpoints = func(r postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint] {
		eps = &loopIter[postgres.Endpoint]{next: func(i int) postgres.Endpoint {
			return postgres.Endpoint{Name: r.Parent + "/endpoints/pending"}
		}}
		return eps
	}
	got, err := NewLakebaseSource("lb", "p1", api, 7).List(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("List = %+v, %v; want no candidates", got, err)
	}
	if eps.n != 7 {
		t.Fatalf("walked %d endpoints, want the cap 7", eps.n)
	}
}
