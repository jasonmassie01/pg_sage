package facts

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// facts-02: max_slot_wal_keep_size is cluster-wide, so a statement that
// sets it bounds every slot of the cluster, whichever slot (or "instance")
// the proposal names. A confirmed slot_consumer fact binds every bound but
// -1 (unbounded).

func cdcSlotFact() []Fact {
	return []Fact{confirmed(9, TypeSlotConsumer, KindSlot, "cdc_*",
		map[string]string{"consumer": "debezium"})}
}

func refStrings(refs []ObjectRef) []string {
	out := []string{}
	for _, r := range refs {
		out = append(out, r.String())
	}
	return out
}

func TestBindSlotConsumerBindsClusterWideWALBoundWhateverItTargets(t *testing.T) {
	sqls := []string{
		"ALTER SYSTEM SET max_slot_wal_keep_size = '1024MB'", // the custodian's form
		"ALTER SYSTEM SET max_slot_wal_keep_size = '10GB'",   // the advisor's form
	}
	targets := [][]string{{"slot:old_replica"}, {"instance"}, nil}
	for _, sql := range sqls {
		for _, target := range targets {
			for _, operator := range []bool{false, true} {
				got := bindSQL(cdcSlotFact(), "", sql, operator, target...)
				if len(got) != 1 || got[0].Fact.ID != 9 || got[0].Route != RouteAlert ||
					got[0].Object != "slot:*" {
					t.Fatalf("%q targets=%v operator=%v: %+v, want fact 9 alert on slot:*",
						sql, target, operator, got)
				}
			}
		}
	}
}

func TestBindSlotConsumerBoundaryUnboundedIsExempt(t *testing.T) {
	free := []string{
		"ALTER SYSTEM SET max_slot_wal_keep_size = -1",
		"ALTER SYSTEM SET max_slot_wal_keep_size = '-1'",
		"ALTER SYSTEM SET max_slot_wal_keep_size TO -1;",
		"ALTER SYSTEM SET max_slot_wal_keep_size = - 1", // PostgreSQL reads -1
		"ALTER SYSTEM SET work_mem = '64MB'",
		"ALTER SYSTEM SET wal_keep_size = '1GB'", // not a slot bound
	}
	for _, sql := range free {
		for _, target := range [][]string{{"slot:old_replica"}, {"instance"}, nil} {
			if got := bindSQL(cdcSlotFact(), "", sql, false, target...); len(got) != 0 {
				t.Fatalf("%q targets=%v must not bind: %+v", sql, target, got)
			}
		}
	}
	bound := []string{
		"ALTER SYSTEM RESET max_slot_wal_keep_size",
		"ALTER SYSTEM RESET ALL",
		"ALTER SYSTEM SET max_slot_wal_keep_size TO DEFAULT",
		"ALTER SYSTEM SET max_slot_wal_keep_size = DEFAULT",
		"ALTER SYSTEM SET max_slot_wal_keep_size = 0",
		"ALTER SYSTEM SET max_slot_wal_keep_size = '1MB'",
		"ALTER SYSTEM SET max_slot_wal_keep_size = '-1MB'", // only an exact -1 is exempt
		"ALTER SYSTEM SET max_slot_wal_keep_size = -1, 5",
		"ALTER SYSTEM SET max_slot_wal_keep_size", // incomplete: conservative
	}
	for _, sql := range bound {
		got := bindSQL(cdcSlotFact(), "", sql, false, "instance")
		if len(got) != 1 || got[0].Route != RouteAlert {
			t.Fatalf("%q: %+v, want one alert binding", sql, got)
		}
	}
}

// Spellings PostgreSQL accepts (quoted or mixed-case name, = or TO, a
// trailing semicolon, a leading comment) all read as an all-slots ref.
func TestResolveRefsAllSlotsSpellings(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{`ALTER SYSTEM SET "MAX_SLOT_WAL_KEEP_SIZE" TO '1GB'`, []string{"slot:*"}},
		{`alter system set Max_Slot_Wal_Keep_Size='1GB';`, []string{"slot:*"}},
		{"ALTER SYSTEM SET max_slot_wal_keep_size = '1GB' ;", []string{"slot:*"}},
		{"/* pg_sage */ ALTER SYSTEM SET max_slot_wal_keep_size = '1GB'",
			[]string{"slot:*"}},
		{"ALTER SYSTEM RESET max_slot_wal_keep_size;", []string{"slot:*"}},
		{`ALTER SYSTEM RESET "max_slot_wal_keep_size"`, []string{"slot:*"}},
		{"ALTER SYSTEM SET max_slot_wal_keep_size = -1;", []string{}},
		{"ALTER SYSTEM SET max_slot_wal_keep_size = '-1';", []string{}},
		{"ALTER SYSTEM SET work_mem = '64MB'", []string{}},
		{"ALTER SYSTEM", []string{}},
		// A -1 followed by another statement is not an unbound.
		{"ALTER SYSTEM SET max_slot_wal_keep_size = -1; " +
			"SELECT pg_drop_replication_slot('cdc_orders')",
			[]string{"slot:*", "slot:cdc_orders"}},
	}
	for _, c := range cases {
		got := refStrings(ResolveRefs(Request{SQL: c.sql}))
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("ResolveRefs(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
	got := refStrings(ResolveRefs(Request{SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '1GB'",
		Targets: []string{"slot:old_replica", "instance"}}))
	if want := []string{"slot:*", "slot:old_replica", "instance"}; !reflect.DeepEqual(got,
		want) {
		t.Fatalf("SQL ref then targets: %v, want %v", got, want)
	}
}

func TestBindSlotConsumerNilAndEmpty(t *testing.T) {
	sql := "ALTER SYSTEM SET max_slot_wal_keep_size = '1024MB'"
	if got := bindSQL(nil, "", sql, false, "instance"); got != nil {
		t.Fatalf("no facts: %+v", got)
	}
	others := []Fact{
		confirmed(1, TypeAppMigrations, KindSchema, "app", nil),
		confirmed(2, TypeAppendOnly, KindTable, "app.audit_log", nil),
		confirmed(3, TypeTestFixture, KindSchema, "test_*", nil),
	}
	// No "instance" target here: how a non-relation target meets schema
	// facts is facts-01's to fix.
	if got := bindSQL(others, "", sql, false); len(got) != 0 {
		t.Fatalf("a WAL bound is not about tables or schemas: %+v", got)
	}
	unconfirmed := cdcSlotFact()
	unconfirmed[0].Status = StatusProposed
	if got := bindSQL(unconfirmed, "", sql, false); len(got) != 0 {
		t.Fatalf("a proposed slot fact bound: %+v", got)
	}
}

// Drop and advance still bind only their named slot.
func TestBindSlotConsumerDropAndAdvanceMatchingUnchanged(t *testing.T) {
	if got := bindSQL(cdcSlotFact(), "", "SELECT pg_drop_replication_slot('old_replica')",
		false, "slot:old_replica"); len(got) != 0 {
		t.Fatalf("dropping an unrelated slot: %+v", got)
	}
	got := bindSQL(cdcSlotFact(), "", "SELECT pg_replication_slot_advance('cdc_x', '0/0')",
		false)
	if len(got) != 1 || got[0].Object != "slot:cdc_x" {
		t.Fatalf("advancing a protected slot: %+v", got)
	}
}

// docs/facts.md promises a slot_consumer fact means pg_sage "never bounds
// WAL in a way that could invalidate it". The promise stays in the docs
// together with its scope, and the binder keeps it for every bound form
// pg_sage proposes.
func TestSlotConsumerDocsPromiseNeverBoundsWAL(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/facts.md")
	if err != nil {
		t.Fatalf("read docs/facts.md: %v", err)
	}
	doc := string(raw)
	var row string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| `slot_consumer`") {
			row = line
		}
	}
	for _, want := range []string{"never bounds WAL in a way that could invalidate it",
		"any finite"} {
		if !strings.Contains(row, want) {
			t.Fatalf("docs/facts.md slot_consumer row %q lacks %q", row, want)
		}
	}
	for _, want := range []string{"max_slot_wal_keep_size", "same cluster",
		"separate sidecar processes"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("docs/facts.md does not say %q about slot facts", want)
		}
	}
	for _, target := range []string{"slot:old_replica", "instance", "slot:cdc_orders"} {
		got := bindSQL(cdcSlotFact(), "", "ALTER SYSTEM SET max_slot_wal_keep_size = '64MB'",
			false, target)
		if len(got) != 1 {
			t.Fatalf("the documented promise is broken for target %s: %+v", target, got)
		}
	}
}

// No concurrent access tests here: ResolveRefs and Bind are pure functions
// of their arguments and share no state.
