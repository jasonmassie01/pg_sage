package facts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// evid-03b: the owner's "tables that must not be touched" bind. A confirmed
// do_not_touch fact on a table, index or schema holds every request on its
// subject, operator-approved or not; only the rollback of pg_sage's own
// change (revert_created_index, restore_dropped_index) stays unbound.

func doNotTouch(id int64, kind Kind, subject string) Fact {
	return confirmed(id, TypeDoNotTouch, kind, subject, map[string]string{"note": "owner"})
}

func TestDoNotTouchValidateKindsAndSubjects(t *testing.T) {
	for _, kind := range []Kind{KindTable, KindIndex, KindSchema} {
		subject := "public.orders"
		if kind == KindSchema {
			subject = "billing"
		}
		p, err := Validate(Proposal{Type: TypeDoNotTouch, Kind: kind, Subject: subject,
			Value: map[string]string{"note": "  owner said so  "}, Source: SourceOperator})
		if err != nil || p.Value["note"] != "owner said so" || p.Subject != subject {
			t.Fatalf("%s %s: %+v %v", kind, subject, p, err)
		}
	}
	for name, c := range map[string]struct {
		kind    Kind
		subject string
		value   map[string]string
		want    error
	}{
		"slot":          {KindSlot, "cdc_orders", nil, ErrInvalidKind},
		"database":      {KindDatabase, "*", nil, ErrInvalidKind},
		"every schema":  {KindTable, "*.*", nil, ErrProtectedSubject},
		"sage schema":   {KindSchema, "sage", nil, ErrProtectedSubject},
		"sage glob":     {KindIndex, "s*.idx_a", nil, ErrProtectedSubject},
		"unqualified":   {KindTable, "orders", nil, ErrInvalidSubject},
		"empty subject": {KindTable, "  ", nil, ErrInvalidSubject},
		"unknown key": {KindTable, "public.orders",
			map[string]string{"window": "x"}, ErrInvalidValue},
		"note too long": {KindTable, "public.orders",
			map[string]string{"note": strings.Repeat("n", 301)}, ErrInvalidValue},
		"catalog schema":  {KindSchema, "pg_catalog", nil, ErrProtectedSubject},
		"wildcard schema": {KindSchema, "*", nil, ErrProtectedSubject},
	} {
		_, err := Validate(Proposal{Type: TypeDoNotTouch, Kind: c.kind, Subject: c.subject,
			Value: c.value, Source: SourceOperator})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

// Every request class on the subject is held, operator-approved or not.
func TestDoNotTouchBindsEveryRequestClass(t *testing.T) {
	f := []Fact{doNotTouch(7, KindTable, "public.orders")}
	for name, c := range map[string]struct {
		action, sql string
		operator    bool
		object      string // the first ref the fact covers
	}{
		"index create": {"create_index_concurrently",
			"CREATE INDEX CONCURRENTLY idx_o ON public.orders (status)", false,
			"public.idx_o"},
		"vacuum":  {"vacuum_table", "VACUUM (ANALYZE) public.orders", false, "public.orders"},
		"analyze": {"analyze_table", "ANALYZE public.orders", false, "public.orders"},
		"approved": {"reindex_index", "REINDEX TABLE CONCURRENTLY public.orders", true,
			"public.orders"},
		"approved ddl": {"alter_table",
			"ALTER TABLE public.orders SET (autovacuum_enabled = off)", true, "public.orders"},
		"delete": {"retention_delete", "DELETE FROM public.orders WHERE id < 10", true,
			"public.orders"},
	} {
		got := bindSQL(f, c.action, c.sql, c.operator)
		if len(got) != 1 || got[0].Fact.ID != 7 || got[0].Route != RouteHold ||
			got[0].Object != c.object {
			t.Errorf("%s: bindings %+v, want fact 7 routed hold on %s", name, got, c.object)
		}
	}
}

// A table fact holds the table's indexes; an index fact holds only that
// index; a schema fact holds everything in it.
func TestDoNotTouchSubjectKinds(t *testing.T) {
	reindex := "REINDEX INDEX CONCURRENTLY public.idx_orders_status"
	table := []Fact{doNotTouch(1, KindTable, "public.orders")}
	req := Request{ActionType: "reindex_index", SQL: reindex, Now: bindNow}
	refs := ResolveRefs(req)
	refs[0].TableSchema, refs[0].TableName = "public", "orders" // as the catalog resolves it
	if got := Bind(table, refs, req); len(got) != 1 || got[0].Route != RouteHold {
		t.Fatalf("table fact on its index: %+v", got)
	}
	index := []Fact{doNotTouch(2, KindIndex, "public.idx_orders_status")}
	if got := bindSQL(index, "reindex_index", reindex, true); len(got) != 1 {
		t.Fatalf("index fact on its index: %+v", got)
	}
	if got := bindSQL(index, "vacuum_table", "VACUUM public.orders", false); got != nil {
		t.Fatalf("an index fact held its table's vacuum: %+v", got)
	}
	schema := []Fact{doNotTouch(3, KindSchema, "billing")}
	if got := bindSQL(schema, "vacuum_table", "VACUUM billing.invoices", true); len(got) != 1 {
		t.Fatalf("schema fact on a table in it: %+v", got)
	}
	if got := bindSQL(schema, "vacuum_table", "VACUUM public.invoices", true); got != nil {
		t.Fatalf("schema fact held another schema: %+v", got)
	}
}

// Rollbacks of pg_sage's own change stay unbound; no facts, an unrelated
// table, an expired or a proposed fact bind nothing.
func TestDoNotTouchExemptionsAndNonBinding(t *testing.T) {
	f := []Fact{doNotTouch(7, KindTable, "public.orders")}
	for _, action := range []string{"revert_created_index", "restore_dropped_index"} {
		if got := bindSQL(f, action, "DROP INDEX CONCURRENTLY public.idx_o", true,
			"public.orders"); got != nil {
			t.Errorf("%s was bound: %+v", action, got)
		}
	}
	vacuum := "VACUUM public.orders"
	if got := bindSQL(nil, "vacuum_table", vacuum, false); got != nil {
		t.Fatalf("no facts bound: %+v", got)
	}
	if got := bindSQL(f, "vacuum_table", "VACUUM public.customers", false); got != nil {
		t.Fatalf("another table was bound: %+v", got)
	}
	expired := f[0]
	expired.Status = StatusExpired
	proposed := f[0]
	proposed.Status = StatusProposed
	past := bindNow.Add(-time.Minute)
	dated := f[0]
	dated.ExpiresAt = &past
	for name, fact := range map[string]Fact{"expired": expired, "proposed": proposed,
		"past expiry": dated} {
		if got := bindSQL([]Fact{fact}, "vacuum_table", vacuum, false); got != nil {
			t.Errorf("%s fact bound: %+v", name, got)
		}
	}
}

func TestDoNotTouchWordsPromptAndPacket(t *testing.T) {
	f := doNotTouch(7, KindTable, "public.orders")
	if got := f.Describe(); got != "table public.orders must not be touched" {
		t.Fatalf("Describe = %q", got)
	}
	s := doNotTouch(8, KindSchema, "billing")
	if got := s.Describe(); got != "schema billing must not be touched" {
		t.Fatalf("schema Describe = %q", got)
	}
	lines := PromptLines([]Fact{f}, bindNow)
	want := "- fact #7 (operator-confirmed): table public.orders must not be touched. " +
		"Never propose any change to it."
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("prompt lines %q, want %q", lines, want)
	}
	b := Binding{Fact: f, Object: "public.orders", Route: RouteHold}
	fix := BuildSourceFix(b, "VACUUM public.orders", "")
	if fix.Route != "hold" || fix.Migration != "" ||
		!strings.Contains(fix.Summary, "must not be touched") ||
		!strings.Contains(fix.Summary, "confirmed by alice@example.com") ||
		!strings.Contains(fix.Summary, "expire the fact") {
		t.Fatalf("hold packet = %+v", fix)
	}
}

// The do-not-touch fact ranks right after the owner brief in prompts, so
// many ownership facts never crowd it out.
func TestDoNotTouchPromptRank(t *testing.T) {
	if !(promptRank(TypeOwnerBrief) < promptRank(TypeDoNotTouch) &&
		promptRank(TypeDoNotTouch) < promptRank(TypeSlotConsumer)) {
		t.Fatalf("ranks: brief %d, do_not_touch %d, slot %d", promptRank(TypeOwnerBrief),
			promptRank(TypeDoNotTouch), promptRank(TypeSlotConsumer))
	}
}

// A finding's change on a do-not-touch table stays visible as advice: the
// executable SQL is withdrawn and the hold packet says why.
func TestDoNotTouchFindingStaysAsAdvice(t *testing.T) {
	f := []Fact{doNotTouch(7, KindTable, "public.orders")}
	in := analyzer.Finding{Category: "table_bloat", ObjectType: "table",
		ObjectIdentifier: "public.orders", Recommendation: "Vacuum it.",
		RecommendedSQL: "VACUUM public.orders"}
	finding := redirect(f, in, ResolveRefs(Request{SQL: in.RecommendedSQL}), bindNow)
	if finding.RecommendedSQL != "" || finding.Detail["bound_by_facts"] == nil ||
		!strings.Contains(finding.Recommendation, "must not be touched") {
		t.Fatalf("finding = %+v", finding)
	}
}

// swapSource returns one of two fact snapshots, switching on every read.
type swapSource struct {
	mu   sync.Mutex
	n    int
	sets [2][]Fact
}

func (s *swapSource) Confirmed(context.Context) ([]Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.sets[s.n%2], nil
}

// A fact expired while decisions are evaluated gives either the held or
// the unbound decision, never a partial one: each Bind reads one snapshot.
func TestDoNotTouchConcurrentExpiryIsAllOrNothing(t *testing.T) {
	held := []Fact{doNotTouch(7, KindTable, "public.orders"),
		doNotTouch(9, KindSchema, "public")}
	src := &swapSource{sets: [2][]Fact{held, nil}}
	binder := NewBinder(src, nil, func() time.Time { return bindNow })
	req := policy.ActionRequest{SQL: "VACUUM public.orders",
		Contract: &policy.ActionContract{ActionType: "vacuum_table"}, OperatorApproved: true}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := binder.Bind(context.Background(), req)
			if err != nil || (len(got) != 0 && len(got) != 2) {
				errs <- "partial or failed decision"
				return
			}
			for _, b := range got {
				if b.Route != string(RouteHold) || b.ConfirmedBy != "alice@example.com" {
					errs <- "binding without its route or confirmer"
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
