package facts

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-01: a target that names no relation ("instance", a database name,
// "llm") never binds; an unqualified target relation binds through the
// schemas the catalog finds it in; SQL-named relations stay the
// conservative floor.

// fakeCatalog is a resolver that also qualifies unqualified targets.
type fakeCatalog struct {
	mu      sync.Mutex
	schemas map[string][]string // relation name -> schemas that have it
	err     error
	asked   [][]string
}

func (c *fakeCatalog) ResolveCatalog(_ context.Context, refs []ObjectRef) ([]ObjectRef,
	error) {
	return append([]ObjectRef(nil), refs...), nil
}

func (c *fakeCatalog) TargetSchemas(_ context.Context, names []string) (
	map[string][]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, append([]string(nil), names...))
	if c.err != nil {
		return nil, c.err
	}
	out := map[string][]string{}
	for _, n := range names {
		if s, ok := c.schemas[n]; ok {
			out[n] = s
		}
	}
	return out, nil
}

// staticFacts is a read-only fact source, safe for concurrent binds.
type staticFacts []Fact

func (s staticFacts) Confirmed(context.Context) ([]Fact, error) { return s, nil }

func fixtureFacts() []Fact {
	return []Fact{
		confirmed(1, TypeTestFixture, KindSchema, "test_*", nil),
		// A wildcard table subject held outside its window (noon is
		// outside 01:00-03:00): it catches any unqualified relation.
		confirmed(2, TypeTableWindow, KindTable, "public.*",
			map[string]string{"kind": "maintenance", "window": "daily 01:00-03:00 UTC"}),
	}
}

func configRequest(sql string, targets ...string) policy.ActionRequest {
	return policy.ActionRequest{SQL: sql, TargetObjs: targets,
		Contract: &policy.ActionContract{ActionType: "alter_system",
			RiskTier: policy.RiskModerate}}
}

func targetBinder(cat *fakeCatalog) *Binder {
	return NewBinder(staticFacts(fixtureFacts()), cat, func() time.Time { return bindNow })
}

func TestBinderTargetsThatNameNoRelationNeverBind(t *testing.T) {
	cat := &fakeCatalog{schemas: map[string][]string{"orders": {"public"}}}
	for _, target := range []string{"instance", "lifeos", "llm"} {
		got, err := targetBinder(cat).Bind(context.Background(),
			configRequest("ALTER SYSTEM SET work_mem='64MB'", target))
		if err != nil || got != nil {
			t.Fatalf("target %q: bindings %+v err %v, want none", target, got, err)
		}
	}
	want := [][]string{{"instance"}, {"lifeos"}, {"llm"}}
	if !reflect.DeepEqual(cat.asked, want) {
		t.Fatalf("resolver asked %v, want %v", cat.asked, want)
	}
}

func TestBinderUnresolvedSQLRelationStillBinds(t *testing.T) {
	cat := &fakeCatalog{}
	got, err := targetBinder(cat).Bind(context.Background(), policy.ActionRequest{
		SQL: "VACUUM events", TargetObjs: []string{"events"},
		Contract: &policy.ActionContract{ActionType: "vacuum_table",
			RiskTier: policy.RiskSafe}})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, b := range got {
		ids = append(ids, b.FactID)
		if b.Object != "events" {
			t.Fatalf("binding object %q, want the SQL-named events", b.Object)
		}
	}
	if !reflect.DeepEqual(ids, []int64{1, 2}) {
		t.Fatalf("unqualified SQL ref must bind both facts, got %+v", got)
	}
}

func TestBinderQualifiesUnqualifiedTargetsBySchema(t *testing.T) {
	cat := &fakeCatalog{schemas: map[string][]string{
		"orders": {"public"}, "shared": {"public", "test_a"}}}
	only, err := targetBinder(cat).Bind(context.Background(),
		configRequest("SELECT 1", "orders"))
	if err != nil {
		t.Fatal(err)
	}
	// public.orders: not a test fixture, but the public.* window holds it.
	if len(only) != 1 || only[0].FactID != 2 || only[0].Object != "public.orders" {
		t.Fatalf("orders: %+v", only)
	}
	both, err := targetBinder(cat).Bind(context.Background(),
		configRequest("SELECT 1", "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 || both[0].FactID != 1 || both[0].Object != "test_a.shared" ||
		both[1].FactID != 2 || both[1].Object != "public.shared" {
		t.Fatalf("shared: %+v", both)
	}
}

func TestBinderIgnoresInvalidTargets(t *testing.T) {
	cat := &fakeCatalog{schemas: map[string][]string{"pid": {"public"}}}
	got, err := targetBinder(cat).Bind(context.Background(),
		configRequest("SELECT pg_terminate_backend(123)", "pid:123", "", "  "))
	if err != nil || got != nil {
		t.Fatalf("invalid targets: %+v %v", got, err)
	}
	if len(cat.asked) != 0 {
		t.Fatalf("resolver asked about invalid targets: %v", cat.asked)
	}
}

func TestBinderLeavesQualifiedAndSlotTargetsAsWritten(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(3, TypeSlotConsumer, KindSlot, "cdc_*", nil),
		confirmed(4, TypeAppMigrations, KindTable, "app.orders", nil)}}
	cat := &fakeCatalog{}
	b := NewBinder(src, cat, func() time.Time { return bindNow })
	got, err := b.Bind(context.Background(), policy.ActionRequest{
		SQL: "SELECT 1", TargetObjs: []string{"slot:cdc_main", "app.orders"},
		Contract: &policy.ActionContract{ActionType: "alter_table",
			RiskTier: policy.RiskModerate}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Object != "slot:cdc_main" || got[1].Object != "app.orders" {
		t.Fatalf("bindings %+v", got)
	}
	if len(cat.asked) != 0 {
		t.Fatalf("qualified and slot targets need no resolution: %v", cat.asked)
	}
}

// Nil-resolver behaviour is unchanged: targets are matched as written, so
// an unqualified target still binds conservatively.
func TestBinderWithoutTargetResolverMatchesTargetsAsWritten(t *testing.T) {
	for name, res := range map[string]RefResolver{"nil": nil,
		"index only": &fakeResolver{}} {
		b := NewBinder(&fakeConfirmed{facts: fixtureFacts()}, res,
			func() time.Time { return bindNow })
		got, err := b.Bind(context.Background(),
			configRequest("ALTER SYSTEM SET work_mem='64MB'", "instance"))
		if err != nil || len(got) != 2 || got[0].Object != "instance" {
			t.Fatalf("%s resolver: %+v %v", name, got, err)
		}
	}
}

func TestBinderTargetResolutionErrorFailsClosed(t *testing.T) {
	boom := errors.New("connection refused")
	cat := &fakeCatalog{err: boom}
	_, err := targetBinder(cat).Bind(context.Background(),
		configRequest("ALTER SYSTEM SET work_mem='64MB'", "instance"))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "resolve target relations") {
		t.Fatalf("resolver error must propagate with context, got %v", err)
	}
	// The gate turning this error into a block is asserted in
	// executor/finding_policy_facts_test.go.
}

func TestBinderTargetResolutionSkippedWithoutFacts(t *testing.T) {
	cat := &fakeCatalog{}
	b := NewBinder(&fakeConfirmed{}, cat, nil)
	got, err := b.Bind(context.Background(), configRequest("SELECT 1", "orders"))
	if err != nil || got != nil || len(cat.asked) != 0 {
		t.Fatalf("no facts: %+v %v, resolver asked %v", got, err, cat.asked)
	}
}

func TestBinderTargetResolutionConcurrent(t *testing.T) {
	cat := &fakeCatalog{schemas: map[string][]string{"shared": {"public", "test_a"}}}
	b := targetBinder(cat)
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := "shared"
			if i%2 == 1 {
				target = "instance"
			}
			got, err := b.Bind(context.Background(), configRequest("SELECT 1", target))
			want := 2
			if target == "instance" {
				want = 0
			}
			if err != nil || len(got) != want {
				errs <- target + ": unexpected bindings"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
