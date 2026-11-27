package facts

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-10: rules see partitions as ordinary tables and propose child-level
// work, so a table fact on a partitioned table must cover its partitions
// (declarative partitioning; legacy inheritance children are not covered).

// No concurrent access tests: the change adds no shared state. The
// resolver is stateless per call and Pattern.Matches is a pure function;
// partitionCatalog locks only so the fake itself is safe.
// No state transition tests: binding is a pure function of the confirmed
// facts and the resolved refs; the fact lifecycle is covered by the store.

// partitionCatalog is a fake catalog: "schema.name" -> what the catalog
// says about that relation.
type partitionCatalog struct {
	mu    sync.Mutex
	rels  map[string]ObjectRef
	err   error
	calls int
	asked []ObjectRef
}

func (c *partitionCatalog) ResolveCatalog(_ context.Context, refs []ObjectRef) (
	[]ObjectRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.asked = append(c.asked, refs...)
	if c.err != nil {
		return nil, c.err
	}
	out := append([]ObjectRef(nil), refs...)
	for i, r := range out {
		got, ok := c.rels[r.Schema+"."+r.Name]
		if r.Schema == "" || !ok {
			continue
		}
		out[i].Kind, out[i].TableSchema, out[i].TableName = got.Kind, got.TableSchema,
			got.TableName
		out[i].RootSchema, out[i].RootName = got.RootSchema, got.RootName
	}
	return out, nil
}

// eventsCatalog is app.events partitioned into app.events_p01 (with an
// index), a sub-partitioned app.events_p02, and a legacy-inheritance
// app.legacy_c1 of app.legacy.
func eventsCatalog() *partitionCatalog {
	child := func(name string) ObjectRef {
		return ObjectRef{Kind: KindTable, RootSchema: "app", RootName: "events", Name: name}
	}
	return &partitionCatalog{rels: map[string]ObjectRef{
		"app.events":        {Kind: KindTable},
		"app.events_p01":    child("events_p01"),
		"app.events_p02":    child("events_p02"),
		"app.events_p02_us": child("events_p02_us"),
		"app.events_p01_ts_idx": {Kind: KindIndex, TableSchema: "app",
			TableName: "events_p01", RootSchema: "app", RootName: "events"},
		"app.legacy_c1": {Kind: KindTable},
		"app.orders":    {Kind: KindTable},
	}}
}

func mustPattern(t *testing.T, kind Kind, raw string) Pattern {
	t.Helper()
	p, err := ParsePattern(kind, raw)
	if err != nil {
		t.Fatalf("pattern %s %q: %v", kind, raw, err)
	}
	return p
}

func TestPatternTableCoversPartitionsThroughTheirRoot(t *testing.T) {
	child := ObjectRef{Kind: KindTable, Schema: "app", Name: "events_p01",
		RootSchema: "app", RootName: "events"}
	idx := ObjectRef{Kind: KindIndex, Schema: "app", Name: "events_p01_ts_idx",
		TableSchema: "app", TableName: "events_p01", RootSchema: "app", RootName: "events"}
	elsewhere := ObjectRef{Kind: KindTable, Schema: "part", Name: "events_p04",
		RootSchema: "app", RootName: "events"}
	legacy := ObjectRef{Kind: KindTable, Schema: "app", Name: "events_p01"}
	other := ObjectRef{Kind: KindTable, Schema: "app", Name: "events_p01",
		RootSchema: "app", RootName: "metrics"}
	cases := []struct {
		kind  Kind
		raw   string
		ref   ObjectRef
		match bool
	}{
		{KindTable, "app.events", child, true},
		{KindTable, "app.events", idx, true},
		{KindTable, "app.ev*", child, true},
		{KindTable, "app.events", elsewhere, true},
		{KindSchema, "app", elsewhere, true}, // the root's schema covers it
		{KindSchema, "part", elsewhere, true},
		{KindSchema, "part", child, false},
		{KindTable, "app.events", legacy, false}, // no root: no match
		{KindTable, "app.events", other, false},
		{KindTable, "other.events", child, false},
		{KindIndex, "app.events", child, false}, // an index pattern never matches a root
		{KindIndex, "app.events_p01_ts_idx", idx, true},
		{KindSlot, "events", child, false},
	}
	for _, c := range cases {
		if got := mustPattern(t, c.kind, c.raw).Matches(c.ref); got != c.match {
			t.Errorf("%s %q matches %+v = %v, want %v", c.kind, c.raw, c.ref, got, c.match)
		}
	}
}

func setAutovacuum(sql string) policy.ActionRequest {
	return policy.ActionRequest{SQL: sql, Contract: &policy.ActionContract{
		ActionType: "set_table_autovacuum", RiskTier: policy.RiskSafe}}
}

func TestBinderBindsPartitionWorkToTheParentsFact(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(31, TypeAppMigrations, KindTable, "app.events", nil),
		confirmed(32, TypeAppendOnly, KindTable, "app.events", nil),
		confirmed(33, TypeTableWindow, KindTable, "app.events",
			map[string]string{"kind": "maintenance", "window": "daily 01:00-03:00 UTC"}),
	}}
	cases := []struct {
		req     policy.ActionRequest
		ids     []int64
		objects []string
	}{
		{setAutovacuum("ALTER TABLE app.events_p01 SET (autovacuum_vacuum_scale_factor=0.01)"),
			[]int64{31, 33}, []string{"app.events_p01", "app.events_p01"}},
		{policyRequest("VACUUM (FULL) app.events_p02_us"), []int64{31, 32, 33},
			[]string{"app.events_p02_us", "app.events_p02_us", "app.events_p02_us"}},
		{policyRequest("DROP INDEX CONCURRENTLY app.events_p01_ts_idx"), []int64{31, 32, 33},
			[]string{"app.events_p01_ts_idx", "app.events_p01_ts_idx",
				"app.events_p01_ts_idx"}},
		{policyRequest("VACUUM (FULL) app.legacy_c1"), nil, nil},
		{policyRequest("VACUUM (FULL) app.orders"), nil, nil},
	}
	for _, c := range cases {
		b := NewBinder(src, eventsCatalog(), func() time.Time { return bindNow })
		got, err := b.Bind(context.Background(), c.req)
		if err != nil {
			t.Fatalf("%s: %v", c.req.SQL, err)
		}
		var ids []int64
		var objects []string
		for _, g := range got {
			ids, objects = append(ids, g.FactID), append(objects, g.Object)
			if g.Subject != "app.events" {
				t.Errorf("%s: binding subject %q", c.req.SQL, g.Subject)
			}
		}
		if !reflect.DeepEqual(ids, c.ids) || !reflect.DeepEqual(objects, c.objects) {
			t.Errorf("%s: bound %v on %v, want %v on %v", c.req.SQL, ids, objects, c.ids,
				c.objects)
		}
	}
}

func TestBinderResolvesQualifiedTableRefsOnly(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(31, TypeAppMigrations, KindTable, "app.events", nil)}}
	res := eventsCatalog()
	b := NewBinder(src, res, func() time.Time { return bindNow })
	// Unqualified refs match as written: no catalog read, any schema.
	got, err := b.Bind(context.Background(), setAutovacuum(
		"ALTER TABLE events SET (fillfactor=90)"))
	if err != nil || len(got) != 1 || got[0].FactID != 31 || res.calls != 0 {
		t.Fatalf("unqualified: %+v %v, resolver calls %d", got, err, res.calls)
	}
	// One batched catalog read per gate call, for every qualified table.
	got, err = b.Bind(context.Background(), setAutovacuum(
		"VACUUM (FULL) app.events_p01, app.orders"))
	if err != nil || len(got) != 1 || res.calls != 1 || len(res.asked) != 2 {
		t.Fatalf("qualified: %+v %v, calls %d asked %+v", got, err, res.calls, res.asked)
	}
	// The root itself resolves with no root and binds once, directly.
	got, err = b.Bind(context.Background(), setAutovacuum(
		"ALTER TABLE app.events SET (fillfactor=90)"))
	if err != nil || len(got) != 1 || got[0].Object != "app.events" {
		t.Fatalf("root: %+v %v", got, err)
	}
}

func TestBinderFailsClosedWhenTableRefsCannotBeResolved(t *testing.T) {
	boom := errors.New("canceling statement due to statement timeout")
	src := &fakeConfirmed{facts: []Fact{
		confirmed(32, TypeAppendOnly, KindTable, "app.events", nil)}}
	b := NewBinder(src, &partitionCatalog{err: boom}, nil)
	for _, sql := range []string{"VACUUM (FULL) app.events_p01",
		"ALTER TABLE app.events_p01 SET (fillfactor=90)", "TRUNCATE app.events_p01"} {
		_, err := b.Bind(context.Background(), policyRequest(sql))
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "resolve") {
			t.Fatalf("%s: resolver error must propagate (facts_unavailable): %v", sql, err)
		}
	}
}

func childFinding(table string) analyzer.Finding {
	return analyzer.Finding{Category: "table_autovacuum", ObjectType: "table",
		ObjectIdentifier: table, Recommendation: "Tune autovacuum.",
		RecommendedSQL: "ALTER TABLE " + table + " SET (autovacuum_vacuum_scale_factor=0.01)",
		RollbackSQL:    "ALTER TABLE " + table + " RESET (autovacuum_vacuum_scale_factor)",
		ActionRisk:     "safe"}
}

func TestFindingFilterRedirectsPartitionFindingsByTheParentsFact(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(31, TypeAppMigrations, KindTable, "app.events", nil)}}
	res := eventsCatalog()
	in := []analyzer.Finding{childFinding("app.events_p01"), childFinding("app.orders"),
		{Category: "unused_index", ObjectIdentifier: "app.events_p01_ts_idx",
			RecommendedSQL: "DROP INDEX CONCURRENTLY app.events_p01_ts_idx"}}
	out, _ := NewFindingFilter(src, res, nil).ApplyFindings(context.Background(), in, nil)
	if len(out) != 3 {
		t.Fatalf("out %+v", out)
	}
	for _, i := range []int{0, 2} {
		ids, _ := out[i].Detail["bound_by_facts"].([]int64)
		if out[i].RecommendedSQL != "" || !reflect.DeepEqual(ids, []int64{31}) {
			t.Fatalf("finding %d on a partition not redirected: %+v", i, out[i])
		}
	}
	if !reflect.DeepEqual(out[1], in[1]) {
		t.Fatalf("finding on an unrelated table changed: %+v", out[1])
	}
	if res.calls != 1 {
		t.Fatalf("one batched catalog read per cycle, got %d", res.calls)
	}
}

func TestFindingFilterKeepsDirectRedirectsWhenTheCatalogFails(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(31, TypeAppMigrations, KindTable, "app.events", nil)}}
	var logged []logLine
	res := &partitionCatalog{err: errors.New("connection refused")}
	f := NewFindingFilter(src, res, capture(&logged))
	in := []analyzer.Finding{childFinding("app.events_p01"), childFinding("app.events")}
	out, _ := f.ApplyFindings(context.Background(), in, nil)
	if !reflect.DeepEqual(out[0], in[0]) {
		t.Fatalf("an unresolved partition finding must pass unchanged (the gate binds): %+v",
			out[0])
	}
	if out[1].RecommendedSQL != "" {
		t.Fatalf("a direct match must still redirect: %+v", out[1])
	}
	if len(logged) != 1 || logged[0].level != "WARN" ||
		!strings.Contains(logged[0].msg, "connection refused") {
		t.Fatalf("logged %+v", logged)
	}
}

func TestFindingFilterReadsNoCatalogWithoutFactsOrResolver(t *testing.T) {
	res := eventsCatalog()
	in := []analyzer.Finding{childFinding("app.events_p01")}
	out, _ := NewFindingFilter(&fakeConfirmed{}, res, nil).ApplyFindings(
		context.Background(), in, nil)
	if !reflect.DeepEqual(out, in) || res.calls != 0 {
		t.Fatalf("no facts: %+v, resolver calls %d", out, res.calls)
	}
	src := &fakeConfirmed{facts: []Fact{
		confirmed(31, TypeAppMigrations, KindTable, "app.events", nil)}}
	out, _ = NewFindingFilter(src, nil, nil).ApplyFindings(context.Background(), in, nil)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("nil resolver matches as written (no root): %+v", out)
	}
}

// shortCatalog breaks the one-ref-per-ref contract.
type shortCatalog struct{}

func (shortCatalog) ResolveCatalog(_ context.Context, refs []ObjectRef) ([]ObjectRef,
	error) {
	return refs[:len(refs)-1], nil
}

func TestFindingFilterRefusesAResolverThatDropsRefs(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(31, TypeAppMigrations, KindTable, "app.events", nil)}}
	var logged []logLine
	in := []analyzer.Finding{childFinding("app.events_p01"), childFinding("app.events")}
	out, _ := NewFindingFilter(src, shortCatalog{}, capture(&logged)).ApplyFindings(
		context.Background(), in, nil)
	if !reflect.DeepEqual(out[0], in[0]) || out[1].RecommendedSQL != "" {
		t.Fatalf("refs must be matched as written: %+v", out)
	}
	if len(logged) != 1 || !strings.Contains(logged[0].msg, "returned 3 refs for 4") {
		t.Fatalf("logged %+v", logged)
	}
}
