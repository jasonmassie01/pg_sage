package provideradvice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEval is the pg_sage side of a reconcile: canned what-if answers and
// index facts, recording what it was asked.
type fakeEval struct {
	mu       sync.Mutex
	act      bool
	reason   string
	facts    map[string]IndexFact
	factErr  error
	whatIfs  []string
	factAsks []string
}

func (f *fakeEval) WhatIf(_ context.Context, ddl string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.whatIfs = append(f.whatIfs, ddl)
	return f.act, f.reason
}

func (f *fakeEval) IndexFact(_ context.Context, index string) (IndexFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.factAsks = append(f.factAsks, index)
	if f.factErr != nil {
		return IndexFact{}, f.factErr
	}
	return f.facts[index], nil
}

var seen = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func createRec(id, ddl string) Recommendation {
	return Recommendation{Provider: "cloud-sql", ProviderID: id, Kind: KindCreateIndex,
		DDL: ddl, Reason: "estimated storage 8 MB", ObservedAt: seen}
}

func missingIndex(id int64, sql string) Finding {
	return Finding{ID: id, Category: "missing_index", Object: "public.users|x", SQL: sql}
}

func onlyRow(t *testing.T, rows []Row) Row {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

func TestReconcile_SameIndexAgreesAndIsRecordedOnce(t *testing.T) {
	ev := &fakeEval{}
	recs := []Recommendation{
		createRec("p1", `CREATE INDEX ON "public"."users"("email")`),
		createRec("p1", `CREATE INDEX ON "public"."users"("email")`), // repeated by the view
	}
	rows := Reconcile(context.Background(), recs,
		[]Finding{missingIndex(7, "CREATE INDEX CONCURRENTLY ON public.users (email);")}, ev)
	r := onlyRow(t, rows)
	if r.Verdict != VerdictAgree || r.FindingID != 7 || r.ProviderID != "p1" ||
		r.Kind != KindCreateIndex {
		t.Fatalf("row = %+v, want agree with finding 7", r)
	}
	if r.Payload.DDL == "" || r.Payload.Object != "public.users" ||
		r.Payload.PGSageSQL != "CREATE INDEX CONCURRENTLY ON public.users (email);" ||
		r.Payload.ProviderReason != "estimated storage 8 MB" || !r.Payload.ObservedAt.Equal(seen) {
		t.Fatalf("payload = %+v", r.Payload)
	}
	if len(ev.whatIfs) != 0 {
		t.Fatalf("an agreed index needs no what-if, got %v", ev.whatIfs)
	}
}

func TestReconcile_UnparseableDDLIsUnparsedAndNeverOfferedAsSQL(t *testing.T) {
	for _, ddl := range []string{
		"CREATE INDEX ON users USING ((",
		"CREATE INDEX ON public.users (email); DROP TABLE public.users",
		"DROP TABLE public.users",
		"",
	} {
		ev := &fakeEval{act: true}
		r := onlyRow(t, Reconcile(context.Background(),
			[]Recommendation{createRec("bad", ddl)}, nil, ev))
		if r.Verdict != VerdictUnparsed || r.Payload.DDL != "" ||
			r.Payload.ProviderText != ddl || r.Payload.WouldAct {
			t.Fatalf("%q: row = %+v, want unparsed with no DDL", ddl, r)
		}
		if !strings.Contains(r.Reason, "not offered as SQL") {
			t.Fatalf("%q: reason %q does not say it is not offered", ddl, r.Reason)
		}
		if len(ev.whatIfs) != 0 {
			t.Fatalf("%q: unparsed DDL reached the what-if", ddl)
		}
	}
}

func TestReconcile_UnknownShapeIsUnknown(t *testing.T) {
	rec := Recommendation{Provider: "cloud-sql", ProviderID: "view-shape", Kind: KindUnknown,
		Reason: "columns (recommendation) are not the known shape", ObservedAt: seen}
	r := onlyRow(t, Reconcile(context.Background(), []Recommendation{rec}, nil, &fakeEval{}))
	if r.Verdict != VerdictUnknown || r.Kind != KindUnknown || r.Payload.DDL != "" ||
		!strings.Contains(r.Reason, "not the known shape") {
		t.Fatalf("row = %+v", r)
	}
}

func TestReconcile_EmptyGivesNoRows(t *testing.T) {
	if rows := Reconcile(context.Background(), nil, nil, &fakeEval{}); len(rows) != 0 {
		t.Fatalf("nil input gave %+v", rows)
	}
	if rows := Reconcile(context.Background(), []Recommendation{}, []Finding{},
		&fakeEval{}); len(rows) != 0 {
		t.Fatalf("empty input gave %+v", rows)
	}
}

func pgsageOnlyFindings() []Finding {
	return []Finding{
		missingIndex(3, "CREATE INDEX CONCURRENTLY ON public.orders (customer_id);"),
		{ID: 4, Category: "unused_index", Object: "public.orders_old_idx",
			SQL: "DROP INDEX CONCURRENTLY public.orders_old_idx;"},
	}
}

func TestReconcile_PGSageOnlyFindingIsListed(t *testing.T) {
	// The provider's advisor is evidently running: it advised another table.
	other := createRec("p9", `CREATE INDEX ON public.events (kind)`)
	rows := Reconcile(context.Background(), []Recommendation{other}, pgsageOnlyFindings(),
		&fakeEval{reason: "would not act: no workload"})
	if len(rows) != 3 || rows[0].ProviderID != "p9" ||
		rows[0].Verdict != VerdictProviderOnly {
		t.Fatalf("rows = %+v", rows)
	}
	want := map[string]Kind{"pgsage:3": KindCreateIndex, "pgsage:4": KindDropIndex}
	for _, r := range rows[1:] {
		if r.Verdict != VerdictPGSageOnly || want[r.ProviderID] != r.Kind ||
			r.FindingID == 0 || r.Payload.PGSageSQL == "" {
			t.Fatalf("row = %+v", r)
		}
	}
}

// No advice (an absent view, an empty list) or only advice pg_sage cannot
// read must not claim the provider's advisor disagrees with pg_sage.
func TestReconcile_NoReadableAdviceClaimsNoPGSageOnly(t *testing.T) {
	unknown := Recommendation{Provider: "cloud-sql", ProviderID: "view-shape",
		Kind: KindUnknown, Reason: "unknown columns", ObservedAt: seen}
	for _, tc := range []struct {
		name     string
		recs     []Recommendation
		wantRows int
	}{
		{"nil", nil, 0},
		{"empty", []Recommendation{}, 0},
		{"unknown only", []Recommendation{unknown}, 1},
		{"no provider id", []Recommendation{
			createRec("", `CREATE INDEX ON public.events (kind)`)}, 0},
	} {
		rows := Reconcile(context.Background(), tc.recs, pgsageOnlyFindings(), &fakeEval{})
		for _, r := range rows {
			if r.Verdict == VerdictPGSageOnly {
				t.Fatalf("%s: pgsage_only row %+v without provider advice", tc.name, r)
			}
		}
		if len(rows) != tc.wantRows {
			t.Fatalf("%s: rows = %+v, want %d", tc.name, rows, tc.wantRows)
		}
	}
}

func TestReconcile_IncludeOnlyDifferenceFollowsCoveredBy(t *testing.T) {
	ev := &fakeEval{act: false, reason: "would not act: HypoPG estimates 2% (< 10%)"}
	pgsage := missingIndex(9, "CREATE INDEX CONCURRENTLY ON public.users (email) INCLUDE (id);")
	r := onlyRow(t, Reconcile(context.Background(),
		[]Recommendation{createRec("p1", "CREATE INDEX ON public.users (email)")},
		[]Finding{pgsage}, ev))
	if r.Verdict != VerdictAgree || r.FindingID != 9 {
		t.Fatalf("pg_sage's INCLUDE index serves the provider's: row = %+v", r)
	}

	narrow := missingIndex(10, "CREATE INDEX CONCURRENTLY ON public.users (email);")
	rows := Reconcile(context.Background(), []Recommendation{
		createRec("p2", "CREATE INDEX ON public.users (email) INCLUDE (name)")},
		[]Finding{narrow}, ev)
	if len(rows) != 2 {
		t.Fatalf("want provider_only plus pgsage_only, got %+v", rows)
	}
	byID := map[string]Row{rows[0].ProviderID: rows[0], rows[1].ProviderID: rows[1]}
	p := byID["p2"]
	if p.Verdict != VerdictProviderOnly || p.FindingID != 0 || p.Payload.WouldAct ||
		p.Reason != ev.reason {
		t.Fatalf("provider's wider INCLUDE is not served: row = %+v", p)
	}
	if byID["pgsage:10"].Verdict != VerdictPGSageOnly {
		t.Fatalf("pg_sage's own index stays pgsage_only: %+v", byID["pgsage:10"])
	}
	if len(ev.whatIfs) != 1 || !strings.Contains(ev.whatIfs[0], "INCLUDE (name)") {
		t.Fatalf("provider_only must go through the what-if, asked %v", ev.whatIfs)
	}
}

func TestReconcile_SameColumnsOnAnotherTableIsNotAgreement(t *testing.T) {
	ev := &fakeEval{act: true, reason: "would act: HypoPG estimates 40% lower cost"}
	rows := Reconcile(context.Background(),
		[]Recommendation{createRec("p1", "CREATE INDEX ON public.accounts (email)")},
		[]Finding{missingIndex(1, "CREATE INDEX CONCURRENTLY ON public.users (email);")}, ev)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.ProviderID == "p1" && (r.Verdict != VerdictProviderOnly || !r.Payload.WouldAct ||
			r.Reason != ev.reason) {
			t.Fatalf("row = %+v", r)
		}
	}
}

func TestReconcile_UnqualifiedTableDefaultsToPublic(t *testing.T) {
	r := onlyRow(t, Reconcile(context.Background(),
		[]Recommendation{createRec("p1", "CREATE INDEX ON users (email)")},
		[]Finding{missingIndex(2, "CREATE INDEX CONCURRENTLY ON public.users (email);")},
		&fakeEval{}))
	if r.Verdict != VerdictAgree || r.FindingID != 2 {
		t.Fatalf("row = %+v", r)
	}
}

func dropRec(id, index string) Recommendation {
	return Recommendation{Provider: "azure", ProviderID: id, Kind: KindDropIndex,
		Object: index, DDL: "DROP INDEX " + index, ObservedAt: seen}
}

func TestReconcile_DropAgreesWithUnusedFinding(t *testing.T) {
	ev := &fakeEval{}
	r := onlyRow(t, Reconcile(context.Background(),
		[]Recommendation{dropRec("d1", "public.users_old_idx")},
		[]Finding{{ID: 5, Category: "unused_index", Object: "public.users_old_idx",
			SQL: "DROP INDEX CONCURRENTLY public.users_old_idx;"}}, ev))
	if r.Verdict != VerdictAgree || r.FindingID != 5 || r.Kind != KindDropIndex {
		t.Fatalf("row = %+v", r)
	}
	if len(ev.factAsks) != 0 {
		t.Fatalf("an agreed drop needs no facts, asked %v", ev.factAsks)
	}
}

func TestReconcile_DropConflicts(t *testing.T) {
	ev := &fakeEval{facts: map[string]IndexFact{
		"public.users_pkey":      {Exists: true, Constraint: "users_pkey"},
		"public.users_email_idx": {Exists: true, Scans: 42},
	}}
	rows := Reconcile(context.Background(), []Recommendation{
		dropRec("d1", "public.users_pkey"), dropRec("d2", "public.users_email_idx")}, nil, ev)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Verdict != VerdictConflict || r.Payload.WouldAct {
			t.Fatalf("row = %+v, want conflict", r)
		}
	}
	if !strings.Contains(rows[0].Reason, "users_pkey") ||
		!strings.Contains(rows[1].Reason, "42 scans") {
		t.Fatalf("reasons = %q / %q", rows[0].Reason, rows[1].Reason)
	}
}

func TestReconcile_DropWithoutPGSageEvidenceIsNotActedOn(t *testing.T) {
	ev := &fakeEval{facts: map[string]IndexFact{
		"public.quiet_idx": {Exists: true},
	}}
	rows := Reconcile(context.Background(), []Recommendation{
		dropRec("d1", "public.quiet_idx"), dropRec("d2", "public.gone_idx")}, nil, ev)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Verdict != VerdictProviderOnly || rows[0].Payload.WouldAct ||
		!strings.HasPrefix(rows[0].Reason, "would not act: ") ||
		!strings.Contains(rows[0].Reason, "unused") {
		t.Fatalf("quiet index row = %+v", rows[0])
	}
	if rows[1].Verdict != VerdictProviderOnly || !strings.Contains(rows[1].Reason, "no longer") {
		t.Fatalf("missing index row = %+v", rows[1])
	}
}

func TestReconcile_DropFactErrorFailsClosed(t *testing.T) {
	ev := &fakeEval{factErr: errors.New("catalog read timed out")}
	r := onlyRow(t, Reconcile(context.Background(),
		[]Recommendation{dropRec("d1", "public.x_idx")}, nil, ev))
	if r.Verdict != VerdictProviderOnly || r.Payload.WouldAct ||
		!strings.Contains(r.Reason, "catalog read timed out") {
		t.Fatalf("row = %+v", r)
	}
}

func TestReconcile_DropOfUnqualifiedOrMalformedNameIsUnparsed(t *testing.T) {
	ev := &fakeEval{}
	r := onlyRow(t, Reconcile(context.Background(),
		[]Recommendation{dropRec("d1", "")}, nil, ev))
	if r.Verdict != VerdictUnparsed || len(ev.factAsks) != 0 {
		t.Fatalf("row = %+v, asked %v", r, ev.factAsks)
	}
}

func TestReconcile_ForeignKeyAdviceMatchesByConstraint(t *testing.T) {
	fk := Finding{ID: 11, Category: "missing_fk_index", Object: "public.orders(customer_id)",
		SQL:        "CREATE INDEX CONCURRENTLY ON public.orders (customer_id);",
		Constraint: "orders_customer_id_fkey"}
	rec := Recommendation{Provider: "supabase", ProviderID: "unindexed_foreign_keys_x",
		Kind: KindCreateIndex, Object: "public.orders", Constraint: "orders_customer_id_fkey"}
	r := onlyRow(t, Reconcile(context.Background(), []Recommendation{rec},
		[]Finding{fk}, &fakeEval{}))
	if r.Verdict != VerdictAgree || r.FindingID != 11 {
		t.Fatalf("row = %+v", r)
	}

	ev := &fakeEval{act: true}
	rec.Constraint = "orders_shop_id_fkey"
	rows := Reconcile(context.Background(), []Recommendation{rec}, nil, ev)
	r = onlyRow(t, rows)
	if r.Verdict != VerdictProviderOnly || r.Payload.WouldAct ||
		!strings.Contains(r.Reason, "no index definition") || len(ev.whatIfs) != 0 {
		t.Fatalf("row = %+v, what-ifs %v", r, ev.whatIfs)
	}
}

func TestReconcile_DuplicateSetAgreesWithAnyDuplicateFinding(t *testing.T) {
	rec := Recommendation{Provider: "supabase", ProviderID: "duplicate_index_x",
		Kind: KindDropIndex, Object: "public.users",
		Indexes: []string{"public.users_a_idx", "public.users_a_idx1"}}
	dup := Finding{ID: 12, Category: "duplicate_index", Object: "public.users_a_idx1",
		SQL: "DROP INDEX CONCURRENTLY public.users_a_idx1;"}
	r := onlyRow(t, Reconcile(context.Background(), []Recommendation{rec},
		[]Finding{dup}, &fakeEval{}))
	if r.Verdict != VerdictAgree || r.FindingID != 12 {
		t.Fatalf("row = %+v", r)
	}
	r = onlyRow(t, Reconcile(context.Background(), []Recommendation{rec}, nil, &fakeEval{}))
	if r.Verdict != VerdictProviderOnly || r.Payload.WouldAct ||
		len(r.Payload.Indexes) != 2 {
		t.Fatalf("row = %+v", r)
	}
}

// The reconcile is evidence only: whatever the provider says, a row never
// carries more than a verdict and a reason (no confidence, risk or action).
func TestReconcile_PayloadCarriesNoAuthority(t *testing.T) {
	ev := &fakeEval{act: true, reason: "would act: HypoPG estimates 50% lower cost"}
	r := onlyRow(t, Reconcile(context.Background(),
		[]Recommendation{createRec("p1", "CREATE INDEX ON public.t (a)")}, nil, ev))
	if r.Verdict != VerdictProviderOnly || !r.Payload.WouldAct {
		t.Fatalf("row = %+v", r)
	}
	raw := string(mustJSON(t, r.Payload))
	for _, banned := range []string{"confidence", "risk", "action_risk", "trust"} {
		if strings.Contains(raw, `"`+banned) {
			t.Fatalf("payload %s carries %q", raw, banned)
		}
	}
}

// No concurrency test here: Reconcile is a pure function of its inputs and
// shares nothing; concurrent writes are tested on the store.
