package facts

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

var bindNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) // a Sunday, noon UTC

func confirmed(id int64, typ Type, kind Kind, subject string, value map[string]string) Fact {
	at := bindNow.Add(-24 * time.Hour)
	return Fact{ID: id, Type: typ, Kind: kind, Subject: subject, Value: value,
		Status: StatusConfirmed, DecidedBy: "alice@example.com", DecidedAt: &at}
}

func bindIDs(bs []Binding) []int64 {
	var out []int64
	for _, b := range bs {
		out = append(out, b.Fact.ID)
	}
	return out
}

func bindSQL(facts []Fact, actionType, sql string, operator bool, targets ...string) []Binding {
	req := Request{ActionType: actionType, SQL: sql, Targets: targets,
		OperatorApproved: operator, Now: bindNow}
	return Bind(facts, ResolveRefs(req), req)
}

func TestResolveRefsFromSQLAndTargets(t *testing.T) {
	idx := func(s, n, ts, tn string) ObjectRef {
		return ObjectRef{Kind: KindIndex, Schema: s, Name: n, TableSchema: ts, TableName: tn}
	}
	tbl := func(s, n string) ObjectRef { return ObjectRef{Kind: KindTable, Schema: s, Name: n} }
	cases := []struct {
		sql     string
		targets []string
		want    []ObjectRef
	}{
		{"CREATE INDEX CONCURRENTLY idx_orders ON public.orders (status)", nil,
			[]ObjectRef{idx("public", "idx_orders", "public", "orders"), tbl("public", "orders")}},
		{`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "Idx" ON ONLY "App"."Orders" (id)`, nil,
			[]ObjectRef{idx("App", "Idx", "App", "Orders"), tbl("App", "Orders")}},
		{"CREATE INDEX ON public.orders (status)", nil, []ObjectRef{tbl("public", "orders")}},
		{"DROP INDEX CONCURRENTLY IF EXISTS public.idx_a, app.idx_b;", nil,
			[]ObjectRef{idx("public", "idx_a", "", ""), idx("app", "idx_b", "", "")}},
		{"ALTER INDEX public.idx_a SET (fillfactor = 90)", nil,
			[]ObjectRef{idx("public", "idx_a", "", "")}},
		{`ALTER TABLE IF EXISTS ONLY "App"."Orders" SET (autovacuum_enabled = on)`, nil,
			[]ObjectRef{tbl("App", "Orders")}},
		{"REINDEX INDEX CONCURRENTLY public.idx_a", nil,
			[]ObjectRef{idx("public", "idx_a", "", "")}},
		{"REINDEX (VERBOSE) TABLE public.orders", nil, []ObjectRef{tbl("public", "orders")}},
		{"VACUUM (FULL, ANALYZE) app.audit_log", nil, []ObjectRef{tbl("app", "audit_log")}},
		{"ANALYZE public.orders", nil, []ObjectRef{tbl("public", "orders")}},
		{"DELETE FROM app.audit_log WHERE at < now() - interval '1 year'", nil,
			[]ObjectRef{tbl("app", "audit_log")}},
		{"TRUNCATE TABLE app.audit_log", nil, []ObjectRef{tbl("app", "audit_log")}},
		{"CREATE STATISTICS IF NOT EXISTS s1 (dependencies) ON a, b FROM public.orders", nil,
			[]ObjectRef{tbl("public", "orders")}},
		{"SELECT pg_drop_replication_slot('cdc_orders')", nil,
			[]ObjectRef{{Kind: KindSlot, Name: "cdc_orders"}}},
		{"SELECT pg_replication_slot_advance('cdc_orders', '0/16B3748')", nil,
			[]ObjectRef{{Kind: KindSlot, Name: "cdc_orders"}}},
		{"ALTER SYSTEM SET max_slot_wal_keep_size = '2048MB'", []string{"slot:cdc_orders"},
			[]ObjectRef{{Kind: KindSlot, Name: "cdc_orders"}}},
		{"", []string{"public.orders|btree(status)", "pid:42"},
			[]ObjectRef{{Kind: KindRelation, Schema: "public", Name: "orders"}}},
	}
	for _, c := range cases {
		got := ResolveRefs(Request{SQL: c.sql, Targets: c.targets})
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("ResolveRefs(%q, %v)\n got %+v\nwant %+v", c.sql, c.targets, got, c.want)
		}
	}
}

func TestBindAppMigrationsIndexNeverCreatedDroppedOrAltered(t *testing.T) {
	facts := []Fact{confirmed(12, TypeAppMigrations, KindIndex, "public.idx_thesis_*", nil)}
	bound := []struct{ action, sql string }{
		{"drop_unused_index", "DROP INDEX CONCURRENTLY public.idx_thesis_allocation_run"},
		{"create_index_concurrently",
			"CREATE INDEX CONCURRENTLY idx_thesis_x ON public.thesis (run_id)"},
		{"alter_table", "ALTER INDEX public.idx_thesis_x SET (fillfactor = 80)"},
		{"", "DROP INDEX public.idx_thesis_x"}, // an unknown action type: the SQL decides
	}
	for _, c := range bound {
		for _, operator := range []bool{false, true} {
			got := bindSQL(facts, c.action, c.sql, operator)
			if len(got) != 1 || got[0].Fact.ID != 12 || got[0].Route != RouteSourceFix {
				t.Fatalf("%q (operator=%v) bindings %+v, want fact 12 source_fix", c.sql,
					operator, got)
			}
		}
	}
	free := []struct{ action, sql string }{
		{"vacuum_table", "VACUUM public.thesis"},
		{"analyze_table", "ANALYZE public.thesis"},
		{"reindex_concurrently", "REINDEX INDEX CONCURRENTLY public.idx_thesis_x"},
		{"drop_unused_index", "DROP INDEX CONCURRENTLY public.idx_other"},
		{"revert_created_index", "DROP INDEX CONCURRENTLY public.idx_thesis_new"},
	}
	for _, c := range free {
		if got := bindSQL(facts, c.action, c.sql, false); len(got) != 0 {
			t.Fatalf("%q must not be bound: %+v", c.sql, got)
		}
	}
}

func TestBindAppMigrationsTableAndSchema(t *testing.T) {
	table := []Fact{confirmed(3, TypeAppMigrations, KindTable, `"App"."Orders"`, nil)}
	if got := bindSQL(table, "create_index_concurrently",
		`CREATE INDEX CONCURRENTLY i1 ON "App"."Orders" (status)`, false); len(got) != 1 {
		t.Fatalf("index on an app-owned table: %+v", got)
	}
	if got := bindSQL(table, "set_table_autovacuum",
		`ALTER TABLE "App"."Orders" SET (autovacuum_vacuum_scale_factor = 0.01)`,
		false); len(got) != 1 {
		t.Fatalf("alter of an app-owned table: %+v", got)
	}
	if got := bindSQL(table, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i1 ON app.orders (status)", false); len(got) != 0 {
		t.Fatalf("app.orders is not \"App\".\"Orders\": %+v", got)
	}
	// A drop names only the index: its parent table (resolved from the
	// catalog) carries the ownership.
	req := Request{ActionType: "drop_unused_index", SQL: `DROP INDEX CONCURRENTLY "App".i1`,
		Now: bindNow}
	refs := []ObjectRef{{Kind: KindIndex, Schema: "App", Name: "i1", TableSchema: "App",
		TableName: "Orders"}}
	if got := Bind(table, refs, req); len(got) != 1 {
		t.Fatalf("drop of an index on an app-owned table: %+v", got)
	}
	schema := []Fact{confirmed(4, TypeAppMigrations, KindSchema, "app", nil)}
	if got := bindSQL(schema, "create_statistics",
		"CREATE STATISTICS s ON a, b FROM app.orders", false); len(got) != 1 {
		t.Fatalf("statistics in an app-owned schema: %+v", got)
	}
}

func TestBindTestFixturesExcludeSelfInitiatedWorkOnly(t *testing.T) {
	facts := []Fact{confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil)}
	for _, c := range []struct{ action, sql string }{
		{"drop_unused_index", "DROP INDEX CONCURRENTLY test_memory_ab12cd.idx_a"},
		{"vacuum_table", "VACUUM test_memory_ab12cd.events"},
		{"create_index_concurrently",
			"CREATE INDEX CONCURRENTLY i ON test_memory_ab12cd.events (a)"},
	} {
		got := bindSQL(facts, c.action, c.sql, false)
		if len(got) != 1 || got[0].Route != RouteExcluded {
			t.Fatalf("%q: %+v, want excluded", c.sql, got)
		}
		if got := bindSQL(facts, c.action, c.sql, true); len(got) != 0 {
			t.Fatalf("an operator's own approval in a fixture schema is not narrowed: %+v", got)
		}
	}
	if got := bindSQL(facts, "vacuum_table", "VACUUM public.events", false); len(got) != 0 {
		t.Fatalf("public is not a fixture: %+v", got)
	}
}

func TestBindSlotConsumerNeverDroppedAdvancedOrBounded(t *testing.T) {
	facts := []Fact{confirmed(9, TypeSlotConsumer, KindSlot, "cdc_*",
		map[string]string{"consumer": "debezium"})}
	for _, operator := range []bool{false, true} {
		for _, c := range []struct {
			sql     string
			targets []string
		}{
			{"SELECT pg_drop_replication_slot('cdc_orders')", []string{"slot:cdc_orders"}},
			{"SELECT pg_replication_slot_advance('cdc_orders', '0/0')", nil},
			{"ALTER SYSTEM SET max_slot_wal_keep_size = '1024MB'", []string{"slot:cdc_orders"}},
		} {
			got := bindSQL(facts, "", c.sql, operator, c.targets...)
			if len(got) != 1 || got[0].Route != RouteAlert {
				t.Fatalf("%q operator=%v: %+v, want alert", c.sql, operator, got)
			}
		}
	}
	if got := bindSQL(facts, "", "SELECT pg_drop_replication_slot('old_replica')", false,
		"slot:old_replica"); len(got) != 0 {
		t.Fatalf("an unrelated slot is not protected: %+v", got)
	}
}

func TestBindAppendOnlyKeepsDataAndIndexes(t *testing.T) {
	facts := []Fact{confirmed(5, TypeAppendOnly, KindTable, "app.audit_log", nil)}
	for _, c := range []struct{ action, sql string }{
		{"retention_delete", "DELETE FROM app.audit_log WHERE at < now() - interval '90 days'"},
		{"", "TRUNCATE app.audit_log"},
		{"plan_bloat_remediation", "VACUUM (FULL) app.audit_log"},
	} {
		got := bindSQL(facts, c.action, c.sql, true)
		if len(got) != 1 || got[0].Route != RouteKeep {
			t.Fatalf("%q: %+v, want keep", c.sql, got)
		}
	}
	// Its indexes look unused because an archive is rarely read.
	req := Request{ActionType: "drop_unused_index", Now: bindNow,
		SQL: "DROP INDEX CONCURRENTLY app.idx_audit_at"}
	refs := []ObjectRef{{Kind: KindIndex, Schema: "app", Name: "idx_audit_at",
		TableSchema: "app", TableName: "audit_log"}}
	if got := Bind(facts, refs, req); len(got) != 1 {
		t.Fatalf("drop of an archive index: %+v", got)
	}
	for _, c := range []struct{ action, sql string }{
		{"vacuum_table", "VACUUM app.audit_log"},
		{"analyze_table", "ANALYZE app.audit_log"},
		{"create_index_concurrently", "CREATE INDEX CONCURRENTLY i ON app.audit_log (at)"},
	} {
		if got := bindSQL(facts, c.action, c.sql, false); len(got) != 0 {
			t.Fatalf("%q must not be bound by append-only: %+v", c.sql, got)
		}
	}
}

func TestBindTableWindows(t *testing.T) {
	maintenance := []Fact{confirmed(20, TypeTableWindow, KindTable, "app.events",
		map[string]string{"kind": "maintenance", "window": "daily 01:00-03:00 UTC"})}
	batch := []Fact{confirmed(21, TypeTableWindow, KindTable, "app.events",
		map[string]string{"kind": "batch", "window": "daily 11:00-13:00 UTC"})}
	at := func(facts []Fact, now time.Time, operator bool) []Binding {
		req := Request{ActionType: "vacuum_table", SQL: "VACUUM app.events", Now: now,
			OperatorApproved: operator}
		return Bind(facts, ResolveRefs(req), req)
	}
	inside := time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC)
	if got := at(maintenance, bindNow, false); len(got) != 1 || got[0].Route != RouteWait {
		t.Fatalf("outside the table's maintenance window: %+v", got)
	}
	if got := at(maintenance, inside, false); len(got) != 0 {
		t.Fatalf("inside the table's maintenance window: %+v", got)
	}
	if got := at(batch, bindNow, false); len(got) != 1 || got[0].Route != RouteWait {
		t.Fatalf("during the batch window: %+v", got)
	}
	if got := at(batch, inside, false); len(got) != 0 {
		t.Fatalf("outside the batch window: %+v", got)
	}
	if got := at(batch, bindNow, true); len(got) != 0 {
		t.Fatalf("an operator chooses their own time: %+v", got)
	}
}

func TestBindIgnoresUnconfirmedAndExpiredFacts(t *testing.T) {
	base := confirmed(1, TypeAppMigrations, KindTable, "app.orders", nil)
	sql := "CREATE INDEX CONCURRENTLY i ON app.orders (a)"
	for _, status := range []Status{StatusProposed, StatusRejected, StatusExpired} {
		f := base
		f.Status = status
		if got := bindSQL([]Fact{f}, "create_index_concurrently", sql, false); len(got) != 0 {
			t.Fatalf("a %s fact bound: %+v", status, got)
		}
	}
	f := base
	past := bindNow.Add(-time.Minute)
	f.ExpiresAt = &past
	if got := bindSQL([]Fact{f}, "create_index_concurrently", sql, false); len(got) != 0 {
		t.Fatalf("a fact past its expiry bound: %+v", got)
	}
	if got := bindSQL(nil, "create_index_concurrently", sql, false); got != nil {
		t.Fatalf("no facts: %+v", got)
	}
}

// Overlapping confirmed patterns each bind once, in fact order; a
// rejected overlapping fact does not.
func TestBindOverlappingPatterns(t *testing.T) {
	facts := []Fact{
		confirmed(30, TypeAppMigrations, KindSchema, "app", nil),
		confirmed(11, TypeAppMigrations, KindTable, "app.ord*", nil),
		confirmed(19, TypeAppMigrations, KindTable, "app.orders", nil),
	}
	facts[2].Status = StatusRejected
	got := bindSQL(facts, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i ON app.orders (a)", false)
	if ids := bindIDs(got); !reflect.DeepEqual(ids, []int64{11, 30}) {
		t.Fatalf("bindings %v, want [11 30]", ids)
	}
}

type fakeConfirmed struct {
	facts []Fact
	err   error
	calls int
}

func (f *fakeConfirmed) Confirmed(context.Context) ([]Fact, error) {
	f.calls++
	return f.facts, f.err
}

type fakeResolver struct {
	parents map[string][2]string
	err     error
	calls   int
}

func (r *fakeResolver) ResolveIndexes(_ context.Context, refs []ObjectRef) ([]ObjectRef,
	error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	out := append([]ObjectRef(nil), refs...)
	for i, ref := range out {
		if p, ok := r.parents[ref.Schema+"."+ref.Name]; ok {
			out[i].Kind, out[i].TableSchema, out[i].TableName = KindIndex, p[0], p[1]
		}
	}
	return out, nil
}

func policyRequest(sql string, targets ...string) policy.ActionRequest {
	return policy.ActionRequest{SQL: sql, TargetObjs: targets,
		Contract: &policy.ActionContract{ActionType: "drop_unused_index",
			RiskTier: policy.RiskSafe}}
}

func TestBinderResolvesParentsAndReportsTheFact(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{confirmed(12, TypeAppMigrations, KindTable,
		"public.thesis", nil)}}
	res := &fakeResolver{parents: map[string][2]string{
		"public.idx_run": {"public", "thesis"}}}
	b := NewBinder(src, res, func() time.Time { return bindNow })
	got, err := b.Bind(context.Background(),
		policyRequest("DROP INDEX CONCURRENTLY public.idx_run", "public.idx_run"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("bindings %+v", got)
	}
	want := policy.FactBinding{FactID: 12, Type: string(TypeAppMigrations),
		Subject: "public.thesis", Route: string(RouteSourceFix),
		Summary:     "table public.thesis is owned by the application's migrations",
		ConfirmedBy: "alice@example.com", ConfirmedAt: bindNow.Add(-24 * time.Hour),
		Object: "public.idx_run"}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("binding\n got %+v\nwant %+v", got[0], want)
	}
}

func TestBinderFailsClosedAndSkipsWorkWithoutFacts(t *testing.T) {
	boom := errors.New("connection refused")
	b := NewBinder(&fakeConfirmed{err: boom}, &fakeResolver{}, nil)
	if _, err := b.Bind(context.Background(), policyRequest("DROP INDEX public.i")); !errors.Is(
		err, boom) {
		t.Fatalf("source error must propagate: %v", err)
	}
	src := &fakeConfirmed{facts: []Fact{confirmed(1, TypeAppMigrations, KindTable,
		"public.t", nil)}}
	b = NewBinder(src, &fakeResolver{err: boom}, nil)
	if _, err := b.Bind(context.Background(), policyRequest("DROP INDEX public.i",
		"public.i")); !errors.Is(err, boom) {
		t.Fatalf("resolver error must propagate: %v", err)
	}
	res := &fakeResolver{}
	b = NewBinder(&fakeConfirmed{}, res, nil)
	got, err := b.Bind(context.Background(), policyRequest("DROP INDEX public.i", "public.i"))
	if err != nil || got != nil || res.calls != 0 {
		t.Fatalf("no facts: %+v %v, resolver calls %d", got, err, res.calls)
	}
}
