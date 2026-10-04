package facts

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func operatorProposal(typ Type, kind Kind, subject string) Proposal {
	return Proposal{Type: typ, Kind: kind, Subject: subject, Source: SourceOperator,
		ProposedBy: "bob@example.com"}
}

func TestStoreProposeDedupesAndMergesEvidence(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p := validProposal()
	first, created, err := s.Propose(ctx, p)
	if err != nil || !created || first.ID == 0 || first.Status != StatusProposed ||
		first.Subject != "public.idx_thesis_allocation_run" || first.Proposals != 1 {
		t.Fatalf("first proposal %+v created=%v err=%v", first, created, err)
	}
	p.Evidence = []Citation{citation("action_log:9")}
	second, created, err := s.Propose(ctx, p)
	if err != nil || created || second.ID != first.ID || second.Proposals != 2 {
		t.Fatalf("second proposal %+v created=%v err=%v", second, created, err)
	}
	if len(second.Evidence) != 2 || second.Evidence[0].Ref != "action_log:9" {
		t.Fatalf("merged evidence %+v (newest first)", second.Evidence)
	}
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.facts").Scan(&rows); err != nil ||
		rows != 1 {
		t.Fatalf("rows %d %v", rows, err)
	}
	bad := p
	bad.Subject = "sage.findings"
	if _, _, err := s.Propose(ctx, bad); !errors.Is(err, ErrProtectedSubject) {
		t.Fatalf("protected subject: %v", err)
	}
}

// Two detectors (or a detector and the model) proposing the same fact at
// once leave one fact, counted twice.
func TestStoreConcurrentProposalsDedupe(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount, ids := 0, map[int64]bool{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, created, err := s.Propose(ctx, validProposal())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("propose: %v", err)
				return
			}
			ids[f.ID] = true
			if created {
				createdCount++
			}
		}()
	}
	wg.Wait()
	f, err := s.Get(ctx, firstKey(ids))
	if err != nil || len(ids) != 1 || createdCount != 1 || f.Proposals != n {
		t.Fatalf("ids %v created %d proposals %d err %v", ids, createdCount, f.Proposals, err)
	}
}

func firstKey(m map[int64]bool) int64 {
	for k := range m {
		return k
	}
	return 0
}

func TestStoreDecideTransitionsAndCache(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Confirmed(ctx); len(got) != 0 {
		t.Fatalf("a proposed fact is not confirmed: %+v", got)
	}
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true}); !errors.Is(err,
		ErrInvalidValue) {
		t.Fatalf("a decision needs an actor: %v", err)
	}
	c, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "alice@example.com",
		Note: "the app's alembic migrations own it"})
	if err != nil || c.Status != StatusConfirmed || c.DecidedBy != "alice@example.com" ||
		c.DecidedAt == nil || c.DecisionNote == "" {
		t.Fatalf("confirm %+v %v", c, err)
	}
	if got, err := s.Confirmed(ctx); err != nil || len(got) != 1 || got[0].ID != f.ID {
		t.Fatalf("confirmed after confirm (cache invalidated): %+v %v", got, err)
	}
	r, err := s.Decide(ctx, f.ID, Decision{Confirm: false, Actor: "carol@example.com"})
	if err != nil || r.Status != StatusRejected || r.DecidedBy != "carol@example.com" {
		t.Fatalf("revoke %+v %v", r, err)
	}
	if got, _ := s.Confirmed(ctx); len(got) != 0 {
		t.Fatalf("a rejected fact still binds: %+v", got)
	}
	if again, err := s.Decide(ctx, f.ID, Decision{Confirm: true,
		Actor: "alice@example.com"}); err != nil || again.Status != StatusConfirmed {
		t.Fatalf("an operator may confirm a rejected fact: %+v %v", again, err)
	}
	if _, err := s.Expire(ctx, f.ID, "index dropped"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true,
		Actor: "alice@example.com"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an expired fact cannot be confirmed: %v", err)
	}
	if _, err := s.Decide(ctx, 987654, Decision{Confirm: true, Actor: "a"}); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("unknown fact: %v", err)
	}
}

// A chat card is bound to the fact it showed: a decision from a stale card
// (the fact changed since it was sent) is refused.
func TestStoreDecideRefusesAStaleHash(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	shown := f.Hash()
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "ui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: false, Actor: "chat",
		ExpectHash: shown}); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale card: %v", err)
	}
	cur, _ := s.Get(ctx, f.ID)
	if cur.Status != StatusConfirmed {
		t.Fatalf("a stale card changed the fact: %+v", cur)
	}
}

func TestStoreReproposalRespectsTheOperatorsDecision(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	slot := Proposal{Type: TypeSlotConsumer, Kind: KindSlot, Subject: "cdc_orders",
		Source: SourceDetector, Value: map[string]string{"consumer": "debezium"},
		Evidence: []Citation{citation("slot:cdc_orders")}}
	f, _, _ := s.Propose(ctx, slot)
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: false, Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	again, created, err := s.Propose(ctx, slot)
	if err != nil || created || again.Status != StatusRejected || again.Proposals != 2 {
		t.Fatalf("a rejected fact must stay rejected: %+v %v %v", again, created, err)
	}
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	slot.Value = map[string]string{"consumer": "airbyte"}
	kept, _, _ := s.Propose(ctx, slot)
	if kept.Status != StatusConfirmed || kept.Value["consumer"] != "debezium" {
		t.Fatalf("a proposal must not rewrite a confirmed fact: %+v", kept)
	}
	if _, err := s.Expire(ctx, f.ID, "slot dropped"); err != nil {
		t.Fatal(err)
	}
	reopened, created, err := s.Propose(ctx, slot)
	if err != nil || !created || reopened.Status != StatusProposed ||
		reopened.DecidedBy != "" || reopened.Value["consumer"] != "airbyte" {
		t.Fatalf("new evidence reopens an expired fact: %+v %v %v", reopened, created, err)
	}
}

func TestStoreDeclareAndExpiry(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p := operatorProposal(TypeAppendOnly, KindTable, "app.audit_log")
	soon := time.Now().Add(time.Hour)
	p.ExpiresAt = &soon
	f, err := s.Declare(ctx, p, "bob@example.com", "archive since 2024")
	if err != nil || f.Status != StatusConfirmed || f.Source != SourceOperator ||
		f.DecidedBy != "bob@example.com" || f.ExpiresAt == nil {
		t.Fatalf("declare %+v %v", f, err)
	}
	if got, _ := s.Confirmed(ctx); len(got) != 1 {
		t.Fatalf("declared fact not confirmed: %+v", got)
	}
	execAll(t, ctx, pool, "UPDATE sage.facts SET expires_at = now() - interval '1 minute'")
	s.invalidate()
	if got, _ := s.Confirmed(ctx); len(got) != 0 {
		t.Fatalf("a fact past its expiry binds: %+v", got)
	}
	rej := operatorProposal(TypeTestFixture, KindSchema, "qa_*")
	rf, _, _ := s.Propose(ctx, Proposal{Type: rej.Type, Kind: rej.Kind, Subject: rej.Subject,
		Source: SourceModel, Evidence: []Citation{citation("schemas:qa_*")}})
	if _, err := s.Decide(ctx, rf.ID, Decision{Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Declare(ctx, rej, "op", ""); err != nil || d.ID != rf.ID ||
		d.Status != StatusConfirmed {
		t.Fatalf("an operator's declaration overrides an earlier rejection: %+v %v", d, err)
	}
}

func TestStoreListFilters(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	a, _, _ := s.Propose(ctx, validProposal())
	if _, err := s.Declare(ctx, operatorProposal(TypeTestFixture, KindSchema, "test_*"),
		"op", ""); err != nil {
		t.Fatal(err)
	}
	all, err := s.List(ctx, Filter{})
	if err != nil || len(all) != 2 || all[0].ID < all[1].ID {
		t.Fatalf("all (newest first): %+v %v", all, err)
	}
	prop, _ := s.List(ctx, Filter{Status: []Status{StatusProposed}})
	if len(prop) != 1 || prop[0].ID != a.ID {
		t.Fatalf("proposed: %+v", prop)
	}
	typed, _ := s.List(ctx, Filter{Type: TypeTestFixture, Limit: 1})
	if len(typed) != 1 || typed[0].Type != TypeTestFixture {
		t.Fatalf("by type: %+v", typed)
	}
	if _, err := s.List(ctx, Filter{Status: []Status{"maybe"}}); !errors.Is(err,
		ErrInvalidValue) {
		t.Fatalf("bad status filter: %v", err)
	}
}

// Confirming a slot consumer also registers it where the WAL custodian
// already looks (sage.slot_consumer_registry), so the custodian escalates
// instead of bounding or dropping the slot.
func TestStoreConfirmedSlotRegistersTheConsumer(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f, _, _ := s.Propose(ctx, Proposal{Type: TypeSlotConsumer, Kind: KindSlot,
		Subject: "cdc_orders", Source: SourceDetector,
		Value: map[string]string{"consumer": "debezium"}, Evidence: []Citation{citation("s")}})
	if _, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	var owner string
	var registered bool
	if err := pool.QueryRow(ctx, `SELECT owner_tag, registered FROM
		sage.slot_consumer_registry WHERE slot_name = 'cdc_orders'`).Scan(&owner,
		&registered); err != nil || owner != "debezium" || !registered {
		t.Fatalf("registry %q %v %v", owner, registered, err)
	}
	// A pattern subject names no single slot: nothing to register.
	g, _, _ := s.Propose(ctx, Proposal{Type: TypeSlotConsumer, Kind: KindSlot,
		Subject: "airbyte_*", Source: SourceOperator,
		Value: map[string]string{"consumer": "airbyte"}})
	if _, err := s.Decide(ctx, g.ID, Decision{Confirm: true, Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM sage.slot_consumer_registry").Scan(&n)
	if n != 1 {
		t.Fatalf("registry rows %d", n)
	}
}

// What operators already declared through register_consumer and
// declare_table_contract becomes confirmed facts, once.
func TestStoreImportsDeclaredContracts(t *testing.T) {
	pool, ctx := livePool(t)
	execAll(t, ctx, pool,
		`INSERT INTO sage.slot_consumer_registry (slot_name, owner_tag, registered)
		 VALUES ('cdc_billing', 'fivetran', true), ('old_slot', 'x', false)`,
		`INSERT INTO sage.table_contract (schema_name, table_name, append_only, declared_by,
		 evidence_id) VALUES ('app', 'audit_log', true, 'mcp:dev', 'ev-import-1'),
		 ('app', 'orders', false, 'mcp:dev', 'ev-import-2')`)
	s := NewStore(pool)
	pre, _, _ := s.Propose(ctx, Proposal{Type: TypeAppendOnly, Kind: KindTable,
		Subject: "app.audit_log", Source: SourceModel, Evidence: []Citation{citation("x")}})
	if _, err := s.Decide(ctx, pre.ID, Decision{Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ImportDeclared(ctx)
	if err != nil || n != 1 {
		t.Fatalf("imported %d %v (the rejected append-only fact is not overridden)", n, err)
	}
	got, _ := s.Confirmed(ctx)
	if len(got) != 1 || got[0].Type != TypeSlotConsumer || got[0].Subject != "cdc_billing" ||
		got[0].Value["consumer"] != "fivetran" || got[0].Source != SourceOperator ||
		got[0].DecidedBy != "register_consumer:fivetran" {
		t.Fatalf("imported facts %+v", got)
	}
	if n, err := s.ImportDeclared(ctx); err != nil || n != 0 {
		t.Fatalf("second import %d %v", n, err)
	}
}
