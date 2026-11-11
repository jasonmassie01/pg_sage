package forecaster

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/runway"
)

// forecast-09: in fleet mode only the cluster owner's forecaster raises
// connection saturation and checkpoint pressure; disk growth, cache
// pressure and query volume stay per database. A non-owner resolves its
// own copies only while the owner reports them.

type fakeClusterOwner struct {
	mu        sync.Mutex
	owner     string
	isOwner   bool
	healthy   bool
	evaluated int
	members   []string
}

func (f *fakeClusterOwner) IsClusterOwner() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owner, f.isOwner
}

func (f *fakeClusterOwner) Evaluated() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evaluated++
}

func (f *fakeClusterOwner) OwnerHealthy(owner string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy && owner == f.owner
}

func (f *fakeClusterOwner) Databases() []string { return f.members }

func ownerFindings() []analyzer.Finding {
	cats := []string{"forecast_disk_growth", "forecast_connection_saturation",
		"forecast_cache_pressure", "forecast_checkpoint_pressure", "forecast_query_volume"}
	out := make([]analyzer.Finding, 0, len(cats))
	for _, c := range cats {
		out = append(out, analyzer.Finding{Category: c, ObjectIdentifier: "instance",
			Detail: map[string]any{"rule": c}})
	}
	return out
}

var allForecastCategories = append(append([]string(nil), systemForecastCategories...),
	queryForecastCategories...)

func categoriesOf(fs []analyzer.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Category)
	}
	return out
}

func TestOwner_NonOwnerRaisesNoClusterForecastAndKeepsDiskGrowth(t *testing.T) {
	o := &fakeClusterOwner{owner: "alpha", members: []string{"alpha", "beta"}}
	f := New(nil, ForecasterConfig{}, nil).WithClusterOwner(o)
	got, cats := f.scopeCluster(ownerFindings(), allForecastCategories)
	want := []string{"forecast_disk_growth", "forecast_cache_pressure",
		"forecast_query_volume"}
	if !slices.Equal(categoriesOf(got), want) {
		t.Fatalf("non-owner findings = %v, want %v", categoriesOf(got), want)
	}
	if slices.Contains(cats, "forecast_connection_saturation") ||
		slices.Contains(cats, "forecast_checkpoint_pressure") {
		t.Fatalf("evaluated %v: an unhealthy owner must leave the copies open", cats)
	}
	if !slices.Contains(cats, "forecast_disk_growth") || o.evaluated != 0 {
		t.Fatalf("evaluated %v (owner records %d): disk growth stays evaluated, a "+
			"non-owner records nothing", cats, o.evaluated)
	}
}

func TestOwner_NonOwnerResolvesCopiesOnlyWhileTheOwnerReports(t *testing.T) {
	o := &fakeClusterOwner{owner: "alpha", healthy: true}
	f := New(nil, ForecasterConfig{}, nil).WithClusterOwner(o)
	// Its own read failed (nothing evaluated): the owner still reports.
	got, cats := f.scopeCluster(nil, nil)
	if len(got) != 0 || !slices.Equal(cats, clusterForecastCategories) {
		t.Fatalf("findings %v evaluated %v, want only the cluster categories", got, cats)
	}
	o.healthy = false
	if _, cats := f.scopeCluster(nil, nil); len(cats) != 0 {
		t.Fatalf("evaluated %v after the owner stopped reporting, want none", cats)
	}
}

func TestOwner_OwnerAnnotatesClusterForecastsAndRecordsACompleteRun(t *testing.T) {
	o := &fakeClusterOwner{owner: "alpha", isOwner: true,
		members: []string{"alpha", "beta", "gamma"}}
	f := New(nil, ForecasterConfig{}, nil).WithClusterOwner(o)
	got, cats := f.scopeCluster(ownerFindings(), allForecastCategories)
	if len(got) != 5 || !slices.Equal(cats, allForecastCategories) {
		t.Fatalf("owner findings %v evaluated %v, want all of them", categoriesOf(got), cats)
	}
	for _, fd := range got {
		scope, _ := fd.Detail["scope"].(string)
		if isClusterForecast(fd.Category) {
			if scope != "cluster" || fd.Detail["owner_database"] != "alpha" ||
				!slices.Equal(fd.Detail["cluster_databases"].([]string), o.members) {
				t.Fatalf("%s detail = %v, want the cluster scope", fd.Category, fd.Detail)
			}
		} else if scope != "" {
			t.Fatalf("%s detail = %v, want no cluster scope", fd.Category, fd.Detail)
		}
		if fd.Detail["rule"] != fd.Category {
			t.Fatalf("%s lost its own detail: %v", fd.Category, fd.Detail)
		}
	}
	if o.evaluated != 1 {
		t.Fatalf("owner recorded %d runs, want 1", o.evaluated)
	}
}

// Boundary: an owner that could not judge checkpoint pressure (legacy
// rows) records no complete run, so no non-owner resolves its copies.
func TestOwner_OwnerWithoutCheckpointHistoryRecordsNothing(t *testing.T) {
	o := &fakeClusterOwner{owner: "alpha", isOwner: true}
	f := New(nil, ForecasterConfig{}, nil).WithClusterOwner(o)
	cats := []string{"forecast_disk_growth", "forecast_connection_saturation"}
	if _, got := f.scopeCluster(nil, cats); !slices.Equal(got, cats) || o.evaluated != 0 {
		t.Fatalf("evaluated %v, owner recorded %d; want %v and 0", got, o.evaluated, cats)
	}
}

// Nil: without an owner every forecast is the database's, as before.
func TestOwner_NilOwnerChangesNothing(t *testing.T) {
	f := New(nil, ForecasterConfig{}, nil)
	in := ownerFindings()
	got, cats := f.scopeCluster(in, allForecastCategories)
	if len(got) != 5 || !slices.Equal(cats, allForecastCategories) ||
		got[1].Detail["scope"] != nil {
		t.Fatalf("findings %v evaluated %v, want them unchanged", categoriesOf(got), cats)
	}
}

// runway.ClusterMember is the forecaster's owner in the sidecar: two
// members of one key agree, and the non-owner resolves its copies once
// the owner has run.
func TestOwner_RunwayClusterMemberDrivesTwoForecasters(t *testing.T) {
	set := runway.NewClusterOwners()
	a := New(nil, ForecasterConfig{}, nil).WithClusterOwner(
		set.Member("sysid:1", "alpha", time.Hour))
	b := New(nil, ForecasterConfig{}, nil).WithClusterOwner(
		set.Member("sysid:1", "beta", time.Hour))
	if got, _ := b.scopeCluster(ownerFindings(), allForecastCategories); len(got) != 5 {
		t.Fatalf("beta alone raised %v, want all five: it owns", categoriesOf(got))
	}
	got, _ := a.scopeCluster(ownerFindings(), allForecastCategories)
	if len(got) != 5 {
		t.Fatalf("alpha (lowest name) raised %v, want all five", categoriesOf(got))
	}
	got, cats := b.scopeCluster(ownerFindings(), allForecastCategories)
	if len(got) != 3 || !slices.Contains(cats, "forecast_checkpoint_pressure") {
		t.Fatalf("beta raised %v evaluated %v, want no cluster forecast and its copies "+
			"resolvable", categoriesOf(got), cats)
	}
}

// No concurrency test here: a forecaster runs on its analyzer goroutine;
// the shared election is raced in internal/runway (owner_test.go).
