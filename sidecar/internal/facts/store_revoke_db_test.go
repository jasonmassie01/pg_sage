package facts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// facts-04: revoking a slot consumer fact (reject or expire) unregisters
// the slot where the WAL custodian looks, so the Facts page and the
// custodian agree; a rejected fact is never resurrected by ImportDeclared.

type registryRow struct {
	exists          bool
	registered      bool
	owner           string
	lastConfirmedAt time.Time
}

func readRegistry(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	slot string) registryRow {
	t.Helper()
	var r registryRow
	err := pool.QueryRow(ctx, `SELECT registered, owner_tag, last_confirmed_at
		FROM sage.slot_consumer_registry WHERE slot_name = $1`, slot).Scan(&r.registered,
		&r.owner, &r.lastConfirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r
	}
	if err != nil {
		t.Fatalf("read registry %s: %v", slot, err)
	}
	r.exists = true
	return r
}

func slotFactProposal(slot, consumer string) Proposal {
	return Proposal{Type: TypeSlotConsumer, Kind: KindSlot, Subject: slot,
		Source: SourceDetector, Value: map[string]string{"consumer": consumer},
		Evidence: []Citation{citation("slot:" + slot)}}
}

// confirmedSlot proposes and confirms a slot fact, so its slot is registered.
func confirmedSlot(t *testing.T, ctx context.Context, s *Store, slot string) Fact {
	t.Helper()
	f, _, err := s.Propose(ctx, slotFactProposal(slot, "debezium"))
	if err != nil {
		t.Fatalf("propose %s: %v", slot, err)
	}
	c, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "alice@example.com"})
	if err != nil || c.Status != StatusConfirmed {
		t.Fatalf("confirm %s: %+v %v", slot, c, err)
	}
	return c
}

func TestStoreRejectUnregistersTheSlotAndConfirmReregisters(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := confirmedSlot(t, ctx, s, "cdc_orders")
	before := readRegistry(t, ctx, pool, "cdc_orders")
	if !before.exists || !before.registered || before.owner != "debezium" {
		t.Fatalf("confirmed slot not registered: %+v", before)
	}
	r, err := s.Decide(ctx, f.ID, Decision{Confirm: false, Actor: "carol@example.com",
		Note: "the CDC pipeline was retired"})
	if err != nil || r.Status != StatusRejected {
		t.Fatalf("reject %+v %v", r, err)
	}
	after := readRegistry(t, ctx, pool, "cdc_orders")
	if !after.exists || after.registered {
		t.Fatalf("a rejected slot fact leaves the slot registered: %+v", after)
	}
	if after.owner != "debezium" || !after.lastConfirmedAt.Equal(before.lastConfirmedAt) {
		t.Fatalf("unregistering must keep owner and last confirmation: %+v vs %+v",
			after, before)
	}
	again, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "alice@example.com"})
	if err != nil || again.Status != StatusConfirmed {
		t.Fatalf("confirm a rejected fact: %+v %v", again, err)
	}
	if got := readRegistry(t, ctx, pool, "cdc_orders"); !got.registered ||
		!got.lastConfirmedAt.After(before.lastConfirmedAt) {
		t.Fatalf("confirming again must re-register: %+v", got)
	}
}

func TestStoreExpireUnregistersTheSlot(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := confirmedSlot(t, ctx, s, "cdc_billing")
	e, err := s.Expire(ctx, f.ID, "ops", "slot dropped")
	if err != nil || e.Status != StatusExpired || e.ExpiredReason != "slot dropped" {
		t.Fatalf("expire %+v %v", e, err)
	}
	if got := readRegistry(t, ctx, pool, "cdc_billing"); !got.exists || got.registered ||
		got.owner != "debezium" {
		t.Fatalf("an expired slot fact leaves the slot registered: %+v", got)
	}
	// Invalid transition: an expired fact cannot expire again, and the
	// refusal leaves the registry alone.
	if _, err := s.Expire(ctx, f.ID, "ops", "again"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expire an expired fact: %v", err)
	}
	if _, err := s.Expire(ctx, 987654, "ops", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expire an unknown fact: %v", err)
	}
	// A proposed slot fact never registered anything: expiring it must
	// not create a registry row.
	p, _, err := s.Propose(ctx, slotFactProposal("cdc_never", "airbyte"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Expire(ctx, p.ID, "ops", "slot dropped"); err != nil {
		t.Fatal(err)
	}
	if got := readRegistry(t, ctx, pool, "cdc_never"); got.exists {
		t.Fatalf("expiring a proposed slot fact created a registry row: %+v", got)
	}
}

// Nil/empty: a pattern subject names no single slot, so neither its
// confirmation nor its rejection touches the registry; a rejected proposal
// with no registry row creates none.
func TestStoreRejectPatternSlotLeavesTheRegistry(t *testing.T) {
	pool, ctx := livePool(t)
	execAll(t, ctx, pool, `INSERT INTO sage.slot_consumer_registry
		(slot_name, owner_tag, registered) VALUES ('airbyte_1', 'mcp:dev', true)`)
	s := NewStore(pool)
	g, _, err := s.Propose(ctx, Proposal{Type: TypeSlotConsumer, Kind: KindSlot,
		Subject: "airbyte_*", Source: SourceOperator,
		Value: map[string]string{"consumer": "airbyte"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, confirm := range []bool{true, false} {
		if _, err := s.Decide(ctx, g.ID, Decision{Confirm: confirm, Actor: "op"}); err != nil {
			t.Fatalf("decide pattern (confirm=%v): %v", confirm, err)
		}
	}
	if got := readRegistry(t, ctx, pool, "airbyte_1"); !got.registered ||
		got.owner != "mcp:dev" {
		t.Fatalf("a pattern rejection changed a registry row: %+v", got)
	}
	p, _, _ := s.Propose(ctx, slotFactProposal("cdc_unseen", "fivetran"))
	if _, err := s.Decide(ctx, p.ID, Decision{Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.slot_consumer_registry").
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("registry rows %d %v (want only airbyte_1)", n, err)
	}
}

// A rejected slot fact is not re-imported, even when register_consumer
// registers the slot again afterwards: that narrows the custodian but
// never confirms the operator's rejection away.
func TestStoreRejectedSlotIsNeverReimported(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := confirmedSlot(t, ctx, s, "cdc_orders")
	if _, err := s.Decide(ctx, f.ID, Decision{Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ImportDeclared(ctx); err != nil || n != 0 {
		t.Fatalf("import after reject %d %v", n, err)
	}
	execAll(t, ctx, pool, `UPDATE sage.slot_consumer_registry SET registered = true
		WHERE slot_name = 'cdc_orders'`)
	if n, err := s.ImportDeclared(ctx); err != nil || n != 0 {
		t.Fatalf("import after a fresh registration %d %v", n, err)
	}
	if cur, err := s.Get(ctx, f.ID); err != nil || cur.Status != StatusRejected {
		t.Fatalf("a rejected fact was confirmed by an import: %+v %v", cur, err)
	}
}

// Boundary: a still-declared append_only contract does not override a
// rejected append-only fact.
func TestStoreRejectedAppendOnlyStaysRejected(t *testing.T) {
	pool, ctx := livePool(t)
	execAll(t, ctx, pool, `INSERT INTO sage.table_contract (schema_name, table_name,
		append_only, declared_by, evidence_id)
		VALUES ('app', 'ledger', true, 'mcp:dev', 'ev-facts04-1')`)
	s := NewStore(pool)
	f, _, err := s.Propose(ctx, Proposal{Type: TypeAppendOnly, Kind: KindTable,
		Subject: "app.ledger", Source: SourceModel, Evidence: []Citation{citation("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, f.ID, Decision{Actor: "op"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ImportDeclared(ctx); err != nil || n != 0 {
		t.Fatalf("import %d %v", n, err)
	}
	if cur, err := s.Get(ctx, f.ID); err != nil || cur.Status != StatusRejected {
		t.Fatalf("append-only fact after import: %+v %v", cur, err)
	}
}

// failRegistryUpdates makes every UPDATE of the registry raise, until the
// test ends.
func failRegistryUpdates(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	execAll(t, ctx, pool, `CREATE OR REPLACE FUNCTION sage.facts04_fail() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'facts04 registry refused'; END $$`,
		`CREATE TRIGGER facts04_fail BEFORE UPDATE ON sage.slot_consumer_registry
		FOR EACH ROW EXECUTE FUNCTION sage.facts04_fail()`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP TRIGGER IF EXISTS facts04_fail ON sage.slot_consumer_registry")
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS sage.facts04_fail()")
	})
}

func TestStoreRevocationRollsBackWhenTheRegistryFails(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := confirmedSlot(t, ctx, s, "cdc_orders")
	failRegistryUpdates(t, ctx, pool)
	_, rejectErr := s.Decide(ctx, f.ID, Decision{Actor: "op"})
	_, expireErr := s.Expire(ctx, f.ID, "ops", "slot dropped")
	for name, err := range map[string]error{"reject": rejectErr, "expire": expireErr} {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Message != "facts04 registry refused" {
			t.Fatalf("%s: error must wrap the registry failure: %v", name, err)
		}
		if !strings.Contains(err.Error(), "slot consumer") ||
			!strings.Contains(err.Error(), "cdc_orders") {
			t.Fatalf("%s: error lacks context: %v", name, err)
		}
	}
	cur, err := s.Get(ctx, f.ID)
	if err != nil || cur.Status != StatusConfirmed || cur.DecidedBy != "alice@example.com" {
		t.Fatalf("a failed revocation changed the fact: %+v %v", cur, err)
	}
	if got := readRegistry(t, ctx, pool, "cdc_orders"); !got.registered {
		t.Fatalf("a failed revocation changed the registry: %+v", got)
	}
	if got, err := s.Confirmed(ctx); err != nil || len(got) != 1 {
		t.Fatalf("the fact still binds after a failed revocation: %+v %v", got, err)
	}
}

// Concurrency: an operator's reject racing the worker's imports ends
// rejected and unregistered, never re-confirmed.
func TestStoreRejectRacesImportDeclared(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := confirmedSlot(t, ctx, s, "cdc_orders")
	var wg sync.WaitGroup
	errs := make(chan error, 11)
	wg.Add(11)
	go func() {
		defer wg.Done()
		_, err := s.Decide(ctx, f.ID, Decision{Actor: "op"})
		errs <- err
	}()
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			_, err := s.ImportDeclared(ctx)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("racing call: %v", err)
		}
	}
	if cur, err := s.Get(ctx, f.ID); err != nil || cur.Status != StatusRejected {
		t.Fatalf("after the race: %+v %v", cur, err)
	}
	if got := readRegistry(t, ctx, pool, "cdc_orders"); !got.exists || got.registered {
		t.Fatalf("registry after the race: %+v", got)
	}
	if n, err := s.ImportDeclared(ctx); err != nil || n != 0 {
		t.Fatalf("import after the race %d %v", n, err)
	}
}

// Safety: an automatic expiry is nobody's decision, so it must not widen
// what the WAL custodian may do. A slot absent past the grace expires its
// fact but stays registered (a consumer that recreates the slot is still
// escalated); a proposed fact's expiry date leaves a register_consumer
// registration alone; only an expiry date a person confirmed unregisters.
func TestReverifyUnregistersOnlyAConfirmedExpiryDate(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool).WithMissingGrace(0)
	gone := confirmedSlot(t, ctx, s, "cdc_gone")
	soon := time.Now().Add(time.Minute)
	datedProposal := slotFactProposal("cdc_dated", "debezium")
	datedProposal.ExpiresAt = &soon
	dated, err := s.Declare(ctx, datedProposal, "alice@example.com", "until the cutover")
	if err != nil {
		t.Fatalf("declare dated: %v", err)
	}
	execAll(t, ctx, pool, `INSERT INTO sage.slot_consumer_registry
		(slot_name, owner_tag, registered) VALUES ('cdc_mcp', 'mcp:dev', true)`)
	mcpProposal := slotFactProposal("cdc_mcp", "fivetran")
	mcpProposal.ExpiresAt = &soon
	proposed, _, err := s.Propose(ctx, mcpProposal)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	got, err := s.Reverify(ctx, time.Now().Add(2*time.Minute))
	if err != nil || len(got) != 3 {
		t.Fatalf("reverify expired %d facts (%v), want 3: %+v", len(got), err, got)
	}
	for _, want := range []struct {
		id         int64
		slot       string
		registered bool
	}{{gone.ID, "cdc_gone", true}, {dated.ID, "cdc_dated", false},
		{proposed.ID, "cdc_mcp", true}} {
		f, err := s.Get(ctx, want.id)
		if err != nil || f.Status != StatusExpired {
			t.Fatalf("%s: %+v %v", want.slot, f, err)
		}
		if r := readRegistry(t, ctx, pool, want.slot); !r.exists ||
			r.registered != want.registered {
			t.Fatalf("%s (%s): registry %+v, want registered=%v", want.slot,
				f.ExpiredReason, r, want.registered)
		}
	}
}
