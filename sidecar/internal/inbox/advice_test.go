package inbox

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// fleetpol-03: the sprawl census's TTL proposals are inbox items of kind
// advice, listed once for the fleet (observe-only members have no
// runtime, so no per-database source can read them).

// fakeCensus answers the census proposals.
type fakeCensus struct {
	props []fleet.CensusProposal
	err   error
	calls atomic.Int32
}

func (f *fakeCensus) CensusProposals(context.Context) ([]fleet.CensusProposal, error) {
	f.calls.Add(1)
	return f.props, f.err
}

func censusProps() []fleet.CensusProposal {
	at := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	return []fleet.CensusProposal{
		{RunID: 4, RunAt: at, DatabaseKey: "a", Owner: "data-team", Creator: "agent:x",
			Proposal: "no real activity in 45 days: propose TTL 7 days"},
		{RunID: 4, RunAt: at, DatabaseKey: "obs-1",
			Proposal: "no real activity in 30 days: propose TTL 7 days"},
	}
}

func TestCensusAdvice_ItemsAreAdviceForAPerson(t *testing.T) {
	src := CensusAdvice(&fakeCensus{props: censusProps()})
	items, err := src.List(context.Background(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("items %+v %v", items, err)
	}
	it := items[0]
	if it.Kind != KindAdvice || it.ID != "census-4-a" || it.Database != "a" ||
		it.Risk != RiskSafe || it.Title != "Set a TTL on a: no real activity in 45 days" ||
		!strings.Contains(it.Why, "propose TTL 7 days") ||
		!strings.Contains(it.Why, "a person decides") ||
		!strings.Contains(it.Why, "owner data-team") ||
		it.Href != "#/manage-databases?database=a" || it.Source != "sage.sprawl_census" ||
		!it.CreatedAt.Equal(censusProps()[0].RunAt) {
		t.Fatalf("item %+v", it)
	}
	if got, err := CensusAdvice(nil).List(context.Background(), ""); got != nil || err != nil {
		t.Fatalf("no census: %v %v, want none", got, err)
	}
	failing := CensusAdvice(&fakeCensus{err: errors.New("meta down")})
	if _, err := failing.List(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "meta down") {
		t.Fatalf("census read failure: %v", err)
	}
	if DeciderRole(KindAdvice) != "operator" || !KindAdvice.Valid() {
		t.Fatal("advice is decided by an operator")
	}
}

func TestService_FleetSourceIsReadOnceAndScopedToTheRequest(t *testing.T) {
	census := &fakeCensus{props: censusProps()}
	s := &Service{Sources: map[Kind]Source{KindAdvice: CensusAdvice(census)}}
	res := s.List(context.Background(), Request{Databases: []string{"a", "b"},
		Kinds: []Kind{KindAdvice}})
	if census.calls.Load() != 1 || res.Total != 1 || res.Items[0].Database != "a" ||
		res.CountsByKind[KindAdvice] != 1 || len(res.Errors) != 0 {
		t.Fatalf("scoped: calls %d result %+v, want a's item only", census.calls.Load(), res)
	}
	res = s.List(context.Background(), Request{Databases: []string{"a"},
		Kinds: []Kind{KindAdvice}, AllDatabases: true})
	if res.Total != 2 || res.Items[1].Database != "obs-1" {
		t.Fatalf("every database: %+v, want both items", res)
	}
	// Nothing readable: a scoped caller with no databases sees no advice.
	if res := s.List(context.Background(), Request{Kinds: []Kind{KindAdvice}}); res.Total != 0 {
		t.Fatalf("no databases: %+v", res)
	}
	failing := &Service{Sources: map[Kind]Source{KindAdvice: CensusAdvice(
		&fakeCensus{err: errors.New("meta down")})}}
	res = failing.List(context.Background(), Request{Databases: []string{"a"},
		Kinds: []Kind{KindAdvice}})
	if len(res.Errors) != 1 || res.Errors[0].Source != "advice" || !res.LowerBound ||
		res.Errors[0].Database != "" {
		t.Fatalf("failing fleet source: %+v", res)
	}
}

// censusFleet is a fleet whose instances read a census.
type censusFleet struct{ *fakeCensus }

func (censusFleet) GetInstance(string) *fleet.DatabaseInstance { return nil }

func TestFleetSources_ListTheFleetsCensusAdvice(t *testing.T) {
	census := &fakeCensus{props: censusProps()}
	s := &Service{Sources: FleetSources(censusFleet{census}, nil)}
	res := s.List(context.Background(), Request{Databases: []string{"a", "b", "c"},
		Kinds: []Kind{KindAdvice}})
	if census.calls.Load() != 1 || res.Total != 1 || res.Items[0].ID != "census-4-a" {
		t.Fatalf("result %+v calls %d, want one read and a's advice", res,
			census.calls.Load())
	}
	// A fleet without a census (or no fleet) lists no advice and no error.
	for _, in := range []Instances{nil, fleet.NewManager(config.DefaultConfig())} {
		res := (&Service{Sources: FleetSources(in, nil)}).List(context.Background(),
			Request{Databases: []string{"a"}, Kinds: []Kind{KindAdvice}, AllDatabases: true})
		if res.Total != 0 || len(res.Errors) != 0 || res.LowerBound {
			t.Fatalf("no census (%T): %+v", in, res)
		}
	}
	// The manager reads the census it was given.
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.SetCensus(census)
	res = (&Service{Sources: FleetSources(mgr, nil)}).List(context.Background(),
		Request{Kinds: []Kind{KindAdvice}, AllDatabases: true})
	if res.Total != 2 {
		t.Fatalf("manager census: %+v, want both proposals", res)
	}
}
