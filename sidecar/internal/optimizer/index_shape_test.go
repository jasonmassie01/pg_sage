package optimizer

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Phase 0 item 7: expression indexes are parsed with the balanced-paren
// DDL scanner (ParseIndexDDL), not by stopping at the first ')', and the
// duplicate check compares the whole index shape.

func TestParseIndexKeys_Expressions(t *testing.T) {
	cases := []struct {
		ddl  string
		want []indexKey
	}{
		{"CREATE INDEX CONCURRENTLY i ON t (a, b DESC NULLS LAST)",
			[]indexKey{{column: "a"}, {column: "b"}}},
		{"CREATE INDEX CONCURRENTLY i ON t ((lower(email)))",
			[]indexKey{{expr: "(lower(email))", refs: []string{"email"}}}},
		{"CREATE INDEX CONCURRENTLY i ON t (lower(email))",
			[]indexKey{{expr: "lower(email)", refs: []string{"email"}}}},
		{"CREATE INDEX CONCURRENTLY i ON t ((payload->>'k'))",
			[]indexKey{{expr: "(payload->>'k')", refs: []string{"payload"}}}},
		{"CREATE INDEX CONCURRENTLY i ON t (date_trunc('day', created_at), id)",
			[]indexKey{{expr: "date_trunc('day',created_at)", refs: []string{"created_at"}},
				{column: "id"}}},
		{"CREATE INDEX CONCURRENTLY i ON t USING gin (payload jsonb_path_ops)",
			[]indexKey{{column: "payload"}}},
		{"CREATE INDEX CONCURRENTLY i ON t (name text_pattern_ops DESC)",
			[]indexKey{{column: "name"}}},
		{`CREATE INDEX CONCURRENTLY i ON t ("Mixed Case", (("Weird"::text)))`,
			[]indexKey{{column: "Mixed Case"},
				{expr: `(("Weird"::text))`, refs: []string{"Weird"}}}},
		{"CREATE INDEX CONCURRENTLY i ON t ((a + b))",
			[]indexKey{{expr: "(a + b)", refs: []string{"a", "b"}}}},
	}
	for _, c := range cases {
		got, err := parseIndexKeys(c.ddl)
		if err != nil {
			t.Errorf("%s: %v", c.ddl, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.ddl, got, c.want)
		}
	}
	for _, bad := range []string{"", "CREATE INDEX i ON t", "CREATE INDEX i ON t ((a)",
		"DROP INDEX i"} {
		if _, err := parseIndexKeys(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// extractColumnsFromDDL returns only plain key columns, so an expression is
// never mistaken for a column named "lower(email".
func TestExtractColumnsFromDDL_SkipsExpressions(t *testing.T) {
	got := extractColumnsFromDDL("CREATE INDEX CONCURRENTLY i ON t (status, (lower(email)))")
	if !reflect.DeepEqual(got, []string{"status"}) {
		t.Fatalf("columns = %v, want [status]", got)
	}
	if got := extractColumnsFromDDL("not ddl"); got != nil {
		t.Fatalf("unparseable = %v", got)
	}
}

func exprTableContext() TableContext {
	tc := sampleTableContext()
	tc.Columns = append(tc.Columns, ColumnInfo{Name: "email", Type: "text"},
		ColumnInfo{Name: "payload", Type: "jsonb"})
	return tc
}

func TestCheckColumnExistence_ExpressionIndexes(t *testing.T) {
	v := newTestValidator(nil)
	tc := exprTableContext()
	for ddl, want := range map[string]bool{
		"CREATE INDEX CONCURRENTLY i ON public.orders ((lower(email)))":                true,
		"CREATE INDEX CONCURRENTLY i ON public.orders ((payload->>'k'))":               true,
		"CREATE INDEX CONCURRENTLY i ON public.orders (date_trunc('day', created_at))": true,
		"CREATE INDEX CONCURRENTLY i ON public.orders ((lower(missing)))":              false,
		"CREATE INDEX CONCURRENTLY i ON public.orders (missing)":                       false,
		"CREATE INDEX CONCURRENTLY i ON public.orders (status) INCLUDE (nope)":         false,
		"CREATE INDEX CONCURRENTLY i ON public.orders (status) INCLUDE (email)":        true,
	} {
		ok, reason := v.checkColumnExistence(Recommendation{DDL: ddl}, tc)
		if ok != want {
			t.Errorf("%s: ok=%t (%s), want %t", ddl, ok, reason, want)
		}
	}
}

// The full validator admits an expression index end to end (it used to be
// rejected as "column lower(email does not exist").
func TestValidate_AdmitsExpressionIndex(t *testing.T) {
	v := newTestValidator(fnTestOptimizerConfig())
	rec := Recommendation{IndexType: "btree",
		DDL: "CREATE INDEX CONCURRENTLY idx_orders_lower_email ON public.orders ((lower(email)))"}
	if ok, reason := v.Validate(context.Background(), rec, exprTableContext()); !ok {
		t.Fatalf("expression index rejected: %s", reason)
	}
}

func dupContext(defs ...IndexInfo) TableContext {
	tc := exprTableContext()
	tc.Indexes = defs
	return tc
}

func validIndex(def string) IndexInfo {
	return IndexInfo{Name: "existing", Definition: def, IsValid: true}
}

var duplicateCases = []struct {
	name     string
	ddl      string
	existing IndexInfo
	dup      bool
}{
	{"same btree", "CREATE INDEX CONCURRENTLY i ON public.orders (status)",
		validIndex("CREATE INDEX existing ON public.orders USING btree (status)"), true},
	{"different method", "CREATE INDEX CONCURRENTLY i ON public.orders USING hash (status)",
		validIndex("CREATE INDEX existing ON public.orders USING btree (status)"), false},
	{"partial vs full", "CREATE INDEX CONCURRENTLY i ON public.orders (status) " +
		"WHERE status = 'open'",
		validIndex("CREATE INDEX existing ON public.orders USING btree (status)"), false},
	{"same predicate, catalog form", "CREATE INDEX CONCURRENTLY i ON public.orders " +
		"(status) WHERE status = 'open'",
		validIndex("CREATE INDEX existing ON public.orders USING btree (status) " +
			"WHERE (status = 'open'::text)"), true},
	{"different predicate", "CREATE INDEX CONCURRENTLY i ON public.orders (status) " +
		"WHERE status = 'closed'",
		validIndex("CREATE INDEX existing ON public.orders USING btree (status) " +
			"WHERE (status = 'open'::text)"), false},
	{"different include", "CREATE INDEX CONCURRENTLY i ON public.orders (status) " +
		"INCLUDE (id)", validIndex("CREATE INDEX existing ON public.orders USING btree " +
		"(status) INCLUDE (created_at)"), false},
	{"same include", "CREATE INDEX CONCURRENTLY i ON public.orders (status) INCLUDE (id)",
		validIndex("CREATE INDEX existing ON public.orders USING btree (status) INCLUDE (id)"),
		true},
	{"expression, catalog form", "CREATE INDEX CONCURRENTLY i ON public.orders " +
		"((lower(email)))", validIndex("CREATE INDEX existing ON public.orders USING btree " +
		"(lower((email)::text))"), true},
	{"expression vs column", "CREATE INDEX CONCURRENTLY i ON public.orders ((lower(email)))",
		validIndex("CREATE INDEX existing ON public.orders USING btree (email)"), false},
	{"invalid existing index ignored", "CREATE INDEX CONCURRENTLY i ON public.orders " +
		"(status)", IndexInfo{Name: "existing", IsValid: false,
		Definition: "CREATE INDEX existing ON public.orders USING btree (status)"}, false},
	{"unparseable existing definition", "CREATE INDEX CONCURRENTLY i ON public.orders " +
		"(status)", validIndex("garbage"), false},
}

func TestCheckDuplicate_ComparesWholeShape(t *testing.T) {
	v := newTestValidator(nil)
	for _, c := range duplicateCases {
		ok, reason := v.checkDuplicate(Recommendation{DDL: c.ddl}, dupContext(c.existing))
		if ok == c.dup {
			t.Errorf("%s: ok=%t (%s), want duplicate=%t", c.name, ok, reason, c.dup)
		}
		if c.dup && !strings.Contains(reason, "existing") {
			t.Errorf("%s: reason %q does not name the index", c.name, reason)
		}
	}
}

func partitionedContext(children ...string) TableContext {
	tc := sampleTableContext()
	tc.Table = "events"
	tc.IsPartitioned = true
	tc.PartitionChildren = children
	return tc
}

// A partitioned parent never gets CREATE INDEX CONCURRENTLY (PostgreSQL
// rejects it): the recommendation becomes an advisory plan — ON ONLY the
// parent, CONCURRENTLY per partition, then ATTACH.
func TestCanonicalize_PartitionedParentIsAdvisory(t *testing.T) {
	rec := sampleRecommendation()
	rec.DDL = "CREATE INDEX CONCURRENTLY idx_events_status ON public.events (status) " +
		"WHERE status <> 'done'"
	tc := partitionedContext("public.events_2026_09", "archive.events_2026_10")
	got, err := canonicalizeRecommendation(rec, tc)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if !got.PartitionedParent || len(got.PartitionPlan) != 5 {
		t.Fatalf("plan = %v (partitioned=%t), want 1 parent + 2x(create, attach)",
			got.PartitionPlan, got.PartitionedParent)
	}
	parent := got.PartitionPlan[0]
	if !strings.Contains(parent, `ON ONLY "public"."events"`) ||
		strings.Contains(parent, "CONCURRENTLY") ||
		!strings.Contains(parent, "WHERE status <> 'done'") {
		t.Fatalf("parent statement = %q", parent)
	}
	seen := map[string]bool{}
	for i, child := range []string{`"public"."events_2026_09"`, `"archive"."events_2026_10"`} {
		create, attach := got.PartitionPlan[1+2*i], got.PartitionPlan[2+2*i]
		if !strings.HasPrefix(create, "CREATE INDEX CONCURRENTLY IF NOT EXISTS ") ||
			!strings.Contains(create, "ON "+child) {
			t.Fatalf("child create = %q", create)
		}
		wantAttach := `ALTER INDEX "public"."idx_events_status" ATTACH PARTITION `
		if !strings.HasPrefix(attach, wantAttach) {
			t.Fatalf("attach = %q", attach)
		}
		name := strings.Fields(create)[6]
		if seen[name] || len(strings.Trim(name, `"`)) > 63 {
			t.Fatalf("child index name %q not unique or too long", name)
		}
		seen[name] = true
	}
}

func TestCanonicalize_PartitionBoundaries(t *testing.T) {
	rec := sampleRecommendation()
	rec.DDL = "CREATE INDEX CONCURRENTLY " + strings.Repeat("x", 63) +
		" ON public.events (status)"
	got, err := canonicalizeRecommendation(rec, partitionedContext("public.events_p1"))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if len(got.PartitionPlan) != 3 {
		t.Fatalf("plan = %v", got.PartitionPlan)
	}
	if name := strings.Trim(strings.Fields(got.PartitionPlan[1])[6], `"`); len(name) > 63 ||
		name == strings.Repeat("x", 63) {
		t.Fatalf("child index name %q: must fit 63 bytes and differ from the parent's", name)
	}
	nested := partitionedContext("public.events_p1")
	nested.NestedPartitions = true
	got, err = canonicalizeRecommendation(sampleRecommendationOn("events"), nested)
	if err != nil || !got.PartitionedParent || got.PartitionPlan != nil {
		t.Fatalf("multi-level partitioning: %+v %v, want advisory without a plan", got, err)
	}
	plain, err := canonicalizeRecommendation(sampleRecommendation(), sampleTableContext())
	if err != nil || plain.PartitionedParent || plain.PartitionPlan != nil {
		t.Fatalf("plain table became partitioned: %+v %v", plain, err)
	}
}

func sampleRecommendationOn(table string) Recommendation {
	rec := sampleRecommendation()
	rec.DDL = "CREATE INDEX CONCURRENTLY idx_" + table + "_status ON public." + table +
		" (status)"
	return rec
}

// The table context lists a parent's direct partitions and flags
// multi-level partitioning (a partition that is itself partitioned).
func TestPartitionChildren(t *testing.T) {
	snap := &collector.Snapshot{Partitions: []collector.PartitionInfo{
		{ParentSchema: "public", ParentTable: "events", ChildSchema: "public",
			ChildTable: "events_b"},
		{ParentSchema: "public", ParentTable: "events", ChildSchema: "archive",
			ChildTable: "events_a"},
		{ParentSchema: "public", ParentTable: "logs", ChildSchema: "public",
			ChildTable: "logs_2026"},
		{ParentSchema: "public", ParentTable: "logs_2026", ChildSchema: "public",
			ChildTable: "logs_2026_10"},
	}}
	children, nested := partitionChildren(snap, "public.events")
	if !reflect.DeepEqual(children, []string{"archive.events_a", "public.events_b"}) ||
		nested {
		t.Fatalf("events = %v nested=%t", children, nested)
	}
	children, nested = partitionChildren(snap, "public.logs")
	if !reflect.DeepEqual(children, []string{"public.logs_2026"}) || !nested {
		t.Fatalf("logs = %v nested=%t, want nested", children, nested)
	}
	if children, nested = partitionChildren(snap, "public.none"); children != nil || nested {
		t.Fatalf("non-parent = %v %t", children, nested)
	}
	if children, _ = partitionChildren(nil, "public.events"); children != nil {
		t.Fatalf("nil snapshot = %v", children)
	}
}
