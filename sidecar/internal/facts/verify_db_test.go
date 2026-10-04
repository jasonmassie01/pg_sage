package facts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/policy"
)

// A fact whose evidence disappeared (its schema, table, index or slot is
// gone) expires once the absence outlasts a grace period, so a fact never
// outlives the objects it was about; an expiry date ends it too.

func TestReverifyExpiresFactsWhoseObjectsAreGone(t *testing.T) {
	pool, ctx := livePool(t)
	schemaName := uniqueName("test_reverify_")
	execAll(t, ctx, pool, "CREATE SCHEMA "+schemaName,
		"CREATE TABLE "+schemaName+".events (id int)",
		"CREATE INDEX idx_rev_events ON "+schemaName+".events (id)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	})
	s := NewStore(pool).WithMissingGrace(time.Hour)
	declare := func(typ Type, kind Kind, subject string, value map[string]string) Fact {
		p := operatorProposal(typ, kind, subject)
		p.Value = value
		f, err := s.Declare(ctx, p, "op", "")
		if err != nil {
			t.Fatalf("declare %s: %v", subject, err)
		}
		return f
	}
	fixture := declare(TypeTestFixture, KindSchema, "test_reverify_*", nil)
	table := declare(TypeAppendOnly, KindTable, schemaName+".events", nil)
	index := declare(TypeAppMigrations, KindIndex, schemaName+".idx_rev_*", nil)
	slot := declare(TypeSlotConsumer, KindSlot, "no_such_slot_anywhere",
		map[string]string{"consumer": "x"})
	t0 := time.Now()
	expired, err := s.Reverify(ctx, t0)
	if err != nil || len(expired) != 0 {
		t.Fatalf("first pass: %+v %v (absence must outlast the grace)", expired, err)
	}
	var verified int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sage.facts WHERE last_verified_at IS NOT
		NULL`).Scan(&verified)
	if verified != 3 {
		t.Fatalf("present facts verified: %d, want 3", verified)
	}
	execAll(t, ctx, pool, "DROP SCHEMA "+schemaName+" CASCADE")
	if got, _ := s.Reverify(ctx, t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("expired inside the grace: %+v", got)
	}
	got, err := s.Reverify(ctx, t0.Add(2*time.Hour))
	if err != nil || len(got) != 4 {
		t.Fatalf("after the grace: %+v %v", got, err)
	}
	for _, id := range []int64{fixture.ID, table.ID, index.ID, slot.ID} {
		f, _ := s.Get(ctx, id)
		if f.Status != StatusExpired || !strings.Contains(f.ExpiredReason, "no ") {
			t.Fatalf("fact %d: %+v", id, f)
		}
	}
}

func TestReverifyHonorsExpiryAndSkipsDecidedNegatives(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p := operatorProposal(TypeAppMigrations, KindSchema, "public")
	soon := time.Now().Add(time.Minute)
	p.ExpiresAt = &soon
	f, err := s.Declare(ctx, p, "op", "")
	if err != nil {
		t.Fatal(err)
	}
	rej, _, _ := s.Propose(ctx, Proposal{Type: TypeTestFixture, Kind: KindSchema,
		Subject: "gone_*", Source: SourceModel, Evidence: []Citation{citation("x")}})
	if _, err := s.Decide(ctx, rej.ID, Decision{Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Reverify(ctx, time.Now().Add(2*time.Minute))
	if err != nil || len(got) != 1 || got[0].ID != f.ID ||
		!strings.Contains(got[0].ExpiredReason, "expiry") {
		t.Fatalf("expiry: %+v %v", got, err)
	}
	r, _ := s.Get(ctx, rej.ID)
	if r.Status != StatusRejected {
		t.Fatalf("a rejected fact was reverified: %+v", r)
	}
}

func TestCatalogResolverFindsIndexParents(t *testing.T) {
	pool, ctx := livePool(t)
	schemaName := uniqueName("resolver_")
	quoted := pgx.Identifier{schemaName}.Sanitize()
	execAll(t, ctx, pool, "CREATE SCHEMA "+quoted,
		`CREATE TABLE `+quoted+`."Orders" (id int)`,
		`CREATE INDEX "Idx_Orders" ON `+quoted+`."Orders" (id)`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
	})
	refs := []ObjectRef{
		{Kind: KindRelation, Schema: schemaName, Name: "Idx_Orders"},
		{Kind: KindIndex, Schema: schemaName, Name: "Idx_Orders"},
		{Kind: KindRelation, Schema: schemaName, Name: "Orders"},
		{Kind: KindRelation, Schema: schemaName, Name: "missing"},
		{Kind: KindSlot, Name: "cdc"},
	}
	got, err := NewCatalogResolver(pool).ResolveIndexes(ctx, refs)
	if err != nil || len(got) != len(refs) {
		t.Fatalf("%+v %v", got, err)
	}
	for _, i := range []int{0, 1} {
		if got[i].Kind != KindIndex || got[i].TableSchema != schemaName ||
			got[i].TableName != "Orders" {
			t.Fatalf("index ref %d: %+v", i, got[i])
		}
	}
	if got[2].Kind != KindTable || got[3].Kind != KindRelation || got[4].Kind != KindSlot {
		t.Fatalf("other refs: %+v", got[2:])
	}
	// End to end: the binder blocks dropping an index of an app-owned table.
	s := NewStore(pool)
	if _, err := s.Declare(ctx, operatorProposal(TypeAppMigrations, KindTable,
		quoted+`."Orders"`), "op", ""); err != nil {
		t.Fatal(err)
	}
	b := NewBinder(s, NewCatalogResolver(pool), nil)
	bound, err := b.Bind(ctx, policy.ActionRequest{
		SQL: `DROP INDEX CONCURRENTLY ` + quoted + `."Idx_Orders"`,
		Contract: &policy.ActionContract{ActionType: "drop_unused_index",
			RiskTier: policy.RiskSafe}})
	if err != nil || len(bound) != 1 || bound[0].Route != string(RouteSourceFix) {
		t.Fatalf("binder: %+v %v", bound, err)
	}
}
