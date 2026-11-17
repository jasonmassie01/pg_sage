package inbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// fixed is a source answering items (one per database, stamped by the
// service) or err.
func fixed(kind Kind, risk string, age time.Duration, err error) Source {
	return SourceFunc(func(_ context.Context, db string) ([]Item, error) {
		if err != nil {
			return nil, err
		}
		return []Item{{Kind: kind, ID: string(kind) + "-" + db, Title: "t",
			Risk: risk, CreatedAt: t0.Add(-age)}}, nil
	})
}

func allFixed() map[Kind]Source {
	m := map[Kind]Source{}
	for i, k := range Kinds() {
		m[k] = fixed(k, RiskModerate, time.Duration(i)*time.Minute, nil)
	}
	return m
}

func TestService_ListsEveryKindAndCountsIt(t *testing.T) {
	s := &Service{Sources: allFixed()}
	res := s.List(context.Background(), Request{Databases: []string{"a", "b"}})
	if res.Total != 14 || len(res.Items) != 14 || res.LowerBound || len(res.Errors) != 0 {
		t.Fatalf("total %d items %d lower %v errors %v, want 14 exact",
			res.Total, len(res.Items), res.LowerBound, res.Errors)
	}
	for _, k := range Kinds() {
		if res.CountsByKind[k] != 2 {
			t.Fatalf("counts_by_kind[%s] = %d, want 2", k, res.CountsByKind[k])
		}
	}
	// Same risk: oldest first, so the last kind (oldest) leads, then a before b.
	if res.Items[0].Kind != KindPolicyException || res.Items[0].Database != "a" ||
		res.Items[1].Database != "b" || res.Items[13].Kind != KindChangeApproval {
		t.Fatalf("order = %+v", res.Items)
	}
}

func TestService_SortsByRiskThenAge(t *testing.T) {
	s := &Service{Sources: map[Kind]Source{
		KindFact:           fixed(KindFact, "safe", time.Hour, nil),
		KindChangeApproval: fixed(KindChangeApproval, "HIGH_RISK", time.Minute, nil),
		KindPlanApproval:   fixed(KindPlanApproval, "", 2*time.Hour, nil),
	}}
	res := s.List(context.Background(), Request{Databases: []string{"a"},
		Kinds: []Kind{KindFact, KindChangeApproval, KindPlanApproval}})
	got := []string{}
	for _, it := range res.Items {
		got = append(got, string(it.Kind)+":"+it.Risk)
	}
	want := "change_approval:high,plan_approval:moderate,fact:safe"
	if strings.Join(got, ",") != want {
		t.Fatalf("order %v, want %s (unknown risk ranks as moderate)", got, want)
	}
}

func TestService_EmptyIsEmptyNotNull(t *testing.T) {
	empty := map[Kind]Source{}
	for _, k := range Kinds() {
		empty[k] = SourceFunc(func(context.Context, string) ([]Item, error) {
			return nil, nil
		})
	}
	res := (&Service{Sources: empty}).List(context.Background(),
		Request{Databases: []string{"a"}})
	if res.Items == nil || res.Errors == nil || res.CountsByKind == nil ||
		res.Total != 0 || res.LowerBound || len(res.CountsByKind) != 7 {
		t.Fatalf("empty result %+v, want [] [] and seven zero counts", res)
	}
	// No databases at all (a scoped user with no grants): still exact zero.
	res = (&Service{Sources: empty}).List(context.Background(), Request{})
	if res.Total != 0 || res.LowerBound || len(res.Errors) != 0 {
		t.Fatalf("no databases: %+v", res)
	}
}

func TestService_NilSourceIsNamedNotConfigured(t *testing.T) {
	src := allFixed()
	src[KindRunbookSignature] = nil
	delete(src, KindFact)
	res := (&Service{Sources: src}).List(context.Background(),
		Request{Databases: []string{"a"}})
	if res.Total != 5 || !res.LowerBound || len(res.Errors) != 2 {
		t.Fatalf("result %+v, want 5 items as a lower bound and two errors", res)
	}
	want := []SourceError{{Source: "fact", Error: "source not configured"},
		{Source: "runbook_signature", Error: "source not configured"}}
	if !reflect.DeepEqual(res.Errors, want) {
		t.Fatalf("errors %+v, want %+v", res.Errors, want)
	}
}

func TestService_TimeoutIsNamedAndOthersStillList(t *testing.T) {
	src := allFixed()
	src[KindAutonomyPromotion] = fixed(KindAutonomyPromotion, "", 0,
		fmt.Errorf("read ledger: %w", context.DeadlineExceeded))
	res := (&Service{Sources: src}).List(context.Background(),
		Request{Databases: []string{"orders"}})
	if res.Total != 6 || !res.LowerBound || res.CountsByKind[KindAutonomyPromotion] != 0 {
		t.Fatalf("result %+v, want the six other kinds as a lower bound", res)
	}
	want := SourceError{Source: "autonomy_promotion", Database: "orders",
		Error: "timed out after 2s"}
	if len(res.Errors) != 1 || res.Errors[0] != want {
		t.Fatalf("errors %+v, want %+v", res.Errors, want)
	}
}

func TestService_ASourceIgnoringItsContextStillTimesOut(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	slow := SourceFunc(func(context.Context, string) ([]Item, error) {
		<-release
		return []Item{{ID: "late"}}, nil
	})
	s := &Service{Sources: map[Kind]Source{KindFact: slow}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	res := s.List(context.Background(), Request{Databases: []string{"a"},
		Kinds: []Kind{KindFact}})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("List took %s, want about the 100ms timeout", took)
	}
	if res.Total != 0 || len(res.Errors) != 1 ||
		res.Errors[0].Error != "timed out after 100ms" {
		t.Fatalf("result %+v, want one timeout error", res)
	}
}

func TestService_AnAnswerJustInsideTheTimeoutIsIncluded(t *testing.T) {
	edge := SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		time.Sleep(150 * time.Millisecond)
		return []Item{{ID: "edge", CreatedAt: t0}}, ctx.Err()
	})
	s := &Service{Sources: map[Kind]Source{KindFact: edge}, Timeout: 400 * time.Millisecond}
	res := s.List(context.Background(), Request{Databases: []string{"a"},
		Kinds: []Kind{KindFact}})
	if res.Total != 1 || res.Items[0].ID != "edge" || res.LowerBound {
		t.Fatalf("result %+v, want the item answered before the timeout", res)
	}
}

func TestService_ErrorWordsNeverLeakTheCause(t *testing.T) {
	src := map[Kind]Source{
		KindFact:         fixed(KindFact, "", 0, errors.New("password=hunter2 refused")),
		KindPlanApproval: fixed(KindPlanApproval, "", 0, ErrNotConnected),
		KindAgentRequest: fixed(KindAgentRequest, "", 0, ErrUnavailable),
		KindChangeApproval: fixed(KindChangeApproval, "", 0,
			fmt.Errorf("x: %w", context.Canceled)),
	}
	res := (&Service{Sources: src}).List(context.Background(), Request{
		Databases: []string{"a"}, Kinds: []Kind{KindFact, KindPlanApproval,
			KindAgentRequest, KindChangeApproval}})
	got := map[string]string{}
	for _, e := range res.Errors {
		got[e.Source] = e.Error
	}
	want := map[string]string{"fact": "read failed; the sidecar log has the cause",
		"plan_approval": "not connected", "agent_request": "unavailable on this database",
		"change_approval": "cancelled"}
	if !reflect.DeepEqual(got, want) || !res.LowerBound {
		t.Fatalf("errors %v, want %v", got, want)
	}
}

func TestService_LimitTruncatesButCountsEverything(t *testing.T) {
	s := &Service{Sources: allFixed()}
	res := s.List(context.Background(), Request{Databases: []string{"a", "b", "c"},
		Limit: 4})
	if len(res.Items) != 4 || res.Total != 21 || !res.Truncated ||
		res.CountsByKind[KindFact] != 3 {
		t.Fatalf("items %d total %d truncated %v, want 4 of 21", len(res.Items),
			res.Total, res.Truncated)
	}
	res = s.List(context.Background(), Request{Databases: []string{"a"}, Limit: 7})
	if len(res.Items) != 7 || res.Truncated {
		t.Fatalf("exactly at the limit: %d items, truncated %v", len(res.Items),
			res.Truncated)
	}
}

func TestService_KindsFilterListsOnlyThose(t *testing.T) {
	s := &Service{Sources: allFixed()}
	res := s.List(context.Background(), Request{Databases: []string{"a"},
		Kinds: []Kind{KindFact}})
	if res.Total != 1 || res.Items[0].Kind != KindFact || len(res.CountsByKind) != 1 {
		t.Fatalf("result %+v, want facts only", res)
	}
}

func TestService_StampsKindDatabaseAndRisk(t *testing.T) {
	bare := SourceFunc(func(context.Context, string) ([]Item, error) {
		return []Item{{ID: "1", Risk: "LOW"}}, nil
	})
	res := (&Service{Sources: map[Kind]Source{KindFact: bare}}).List(
		context.Background(), Request{Databases: []string{"orders"},
			Kinds: []Kind{KindFact}})
	it := res.Items[0]
	if it.Kind != KindFact || it.Database != "orders" || it.Risk != RiskSafe ||
		it.Actionable {
		t.Fatalf("item %+v, want stamped fact/orders/safe, not actionable", it)
	}
}

// Concurrency: -race is OWNER ACTION (no cgo here); twenty parallel Lists
// on one service must give identical results.
func TestService_ParallelListsShareNothing(t *testing.T) {
	s := &Service{Sources: allFixed(), Workers: 3}
	req := Request{Databases: []string{"a", "b", "c"}}
	first := s.List(context.Background(), req)
	var wg sync.WaitGroup
	results := make([]Result, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.List(context.Background(), req)
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if !reflect.DeepEqual(r, first) {
			t.Fatalf("parallel list %d differs: %+v vs %+v", i, r, first)
		}
	}
}

func TestService_ManyDatabasesStayBounded(t *testing.T) {
	dbs := make([]string, 500)
	for i := range dbs {
		dbs[i] = fmt.Sprintf("db%03d", i)
	}
	two := SourceFunc(func(_ context.Context, db string) ([]Item, error) {
		return []Item{{ID: db + "-1"}, {ID: db + "-2"}}, nil
	})
	s := &Service{Sources: map[Kind]Source{KindFact: two}}
	start := time.Now()
	res := s.List(context.Background(), Request{Databases: dbs, Kinds: []Kind{KindFact},
		Limit: MaxLimit})
	if res.Total != 1000 || len(res.Items) != 1000 {
		t.Fatalf("total %d, want 1000", res.Total)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("500 databases took %s", took)
	}
}
