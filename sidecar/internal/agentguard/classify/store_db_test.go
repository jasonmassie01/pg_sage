package classify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentguard/classify"))
}

// livePool bootstraps the fixture, empties the class facts and creates a
// fresh schema with one users table.
func livePool(t *testing.T) (*pgxpool.Pool, context.Context, string) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	clean := func() { _, _ = pool.Exec(context.Background(), "DELETE FROM sage.facts") }
	clean()
	t.Cleanup(clean)
	sch := fmt.Sprintf("cls%06x", time.Now().UnixNano()&0xffffff)
	exec(t, ctx, pool, `CREATE SCHEMA `+sch,
		`CREATE TABLE `+sch+`.users (id bigint PRIMARY KEY, email text, password text,
		 bio text, nickname text)`,
		`COMMENT ON COLUMN `+sch+`.users.nickname IS 'shown publicly'`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA `+sch+` CASCADE`)
	})
	return pool, ctx, sch
}

func exec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func resolve(t *testing.T, ctx context.Context, s *Store, sch, column string) Column {
	t.Helper()
	c, err := s.ResolveColumn(ctx, sch, "users", column)
	if err != nil {
		t.Fatalf("resolve %s: %v", column, err)
	}
	return c
}

func TestStore_SetAndLookup(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	email := resolve(t, ctx, s, sch, "email")
	if email.RelID == 0 || email.AttNum != 2 || email.Type != "text" {
		t.Fatalf("resolved %+v", email)
	}
	got, err := s.Set(ctx, email, ClassPII, "alice@example.com", "contact data")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusConfirmed || got.Class != ClassPII || got.Source != SourceOperator ||
		got.DecidedBy != "alice@example.com" || got.DecidedAt == nil {
		t.Fatalf("set: %+v", got)
	}
	eff, err := s.ClassOf(ctx, email.RelID, email.AttNum)
	if err != nil || eff.Class != ClassPII || !eff.Confirmed {
		t.Fatalf("class of email: %+v %v", eff, err)
	}
	bio := resolve(t, ctx, s, sch, "bio")
	if eff, err := s.ClassOf(ctx, bio.RelID, bio.AttNum); err != nil ||
		eff.Class != Unclassified || eff.Confirmed {
		t.Fatalf("bio is unclassified: %+v %v", eff, err)
	}
	again, err := s.Set(ctx, email, ClassSecret, "bob@example.com", "")
	if err != nil || again.ID != got.ID || again.Class != ClassSecret ||
		again.DecidedBy != "bob@example.com" {
		t.Fatalf("operator re-classification: %+v %v", again, err)
	}
	// The generic facts store never shows column classes (they have their
	// own API) and never binds them in the gate.
	listed, err := facts.NewStore(pool).List(ctx, facts.Filter{})
	if err != nil || len(listed) != 0 {
		t.Fatalf("facts list leaked column classes: %d %v", len(listed), err)
	}
	confirmed, err := facts.NewStore(pool).Confirmed(ctx)
	if err != nil || len(confirmed) != 0 {
		t.Fatalf("facts confirmed leaked column classes: %d %v", len(confirmed), err)
	}
	if _, err := facts.NewStore(pool).Get(ctx, got.ID); !errors.Is(err, facts.ErrNotFound) {
		t.Fatalf("facts get of a column class: %v", err)
	}
}

func TestStore_RenameKeepsClass(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	pw := resolve(t, ctx, s, sch, "password")
	if _, err := s.Set(ctx, pw, ClassSecret, "alice@example.com", ""); err != nil {
		t.Fatal(err)
	}
	exec(t, ctx, pool, `ALTER TABLE `+sch+`.users RENAME COLUMN password TO pw_digest`,
		`ALTER TABLE `+sch+`.users RENAME TO accounts`)
	rc, err := s.Lookup(ctx, pw.RelID)
	if err != nil {
		t.Fatal(err)
	}
	c := rc.Columns[pw.AttNum]
	if c.Class != ClassSecret || c.Column.Name != "pw_digest" || c.Column.Table != "accounts" {
		t.Fatalf("after rename: %+v", c)
	}
}

func TestStore_DroppedColumnExpires(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	nick := resolve(t, ctx, s, sch, "nickname")
	if _, err := s.Set(ctx, nick, ClassClean, "alice@example.com", ""); err != nil {
		t.Fatal(err)
	}
	exec(t, ctx, pool, `ALTER TABLE `+sch+`.users DROP COLUMN nickname`,
		`ALTER TABLE `+sch+`.users ADD COLUMN nickname text`)
	n, err := s.ExpireDropped(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expire dropped: %d %v", n, err)
	}
	eff, err := s.ClassOf(ctx, nick.RelID, nick.AttNum)
	if err != nil || eff.Class != Unclassified {
		t.Fatalf("dropped column's class must not survive: %+v %v", eff, err)
	}
	fresh := resolve(t, ctx, s, sch, "nickname")
	if fresh.AttNum == nick.AttNum {
		t.Fatal("a re-added column gets a new attnum")
	}
	if eff, _ := s.ClassOf(ctx, fresh.RelID, fresh.AttNum); eff.Class != Unclassified {
		t.Fatalf("a re-added column starts unclassified: %+v", eff)
	}
	if n, err := s.ExpireDropped(ctx); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
}

func detectorProposal(c Column, class Class) Proposal {
	return Proposal{Column: c, Class: class, Source: SourceDetector, ProposedBy: RulesProposer,
		Evidence: []Citation{{Kind: "column", Ref: c.QualifiedName(), Detail: "name"}},
		Rationale: "name heuristic"}
}

func TestStore_ProposeDecide(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	bio := resolve(t, ctx, s, sch, "bio")
	p, created, err := s.Propose(ctx, detectorProposal(bio, ClassUntrusted))
	if err != nil || !created || p.Status != StatusProposed || p.Proposals != 1 {
		t.Fatalf("propose: %+v %v %v", p, created, err)
	}
	if eff, _ := s.ClassOf(ctx, bio.RelID, bio.AttNum); eff.Class != ClassUntrusted ||
		eff.Confirmed {
		t.Fatalf("a narrowing proposal counts before confirmation: %+v", eff)
	}
	again, created, err := s.Propose(ctx, detectorProposal(bio, ClassUntrusted))
	if err != nil || created || again.ID != p.ID || again.UpdatedAt != p.UpdatedAt {
		t.Fatalf("an identical re-proposal writes nothing: %+v %v %v", again, created, err)
	}
	if _, err := s.Decide(ctx, p.ID, true, "alice@example.com", "", "stale"); !errors.Is(
		err, ErrChanged) {
		t.Fatalf("stale hash: %v", err)
	}
	ok, err := s.Decide(ctx, p.ID, true, "alice@example.com", "yes", p.Hash())
	if err != nil || ok.Status != StatusConfirmed || ok.DecidedBy != "alice@example.com" {
		t.Fatalf("confirm: %+v %v", ok, err)
	}
	if _, err := s.Decide(ctx, p.ID, false, "bob@example.com", "", ""); !errors.Is(err,
		ErrInvalidTransition) {
		t.Fatalf("deciding a confirmed class again: %v", err)
	}
	// A later proposal never overrides a confirmed decision.
	if c, _, err := s.Propose(ctx, detectorProposal(bio, ClassPII)); err != nil ||
		c.Class != ClassUntrusted || c.Status != StatusConfirmed {
		t.Fatalf("proposal over a confirmed class: %+v %v", c, err)
	}
	if _, err := s.Decide(ctx, 987654321, true, "a@example.com", "", ""); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestStore_RejectThenDifferentClassReopens(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	nick := resolve(t, ctx, s, sch, "nickname")
	p, _, err := s.Propose(ctx, detectorProposal(nick, ClassPII))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, p.ID, false, "alice@example.com", "public handle", ""); err != nil {
		t.Fatal(err)
	}
	if eff, _ := s.ClassOf(ctx, nick.RelID, nick.AttNum); eff.Class != Unclassified {
		t.Fatalf("a rejected proposal does not narrow: %+v", eff)
	}
	same, created, err := s.Propose(ctx, detectorProposal(nick, ClassPII))
	if err != nil || created || same.Status != StatusRejected {
		t.Fatalf("the same class again stays rejected: %+v %v %v", same, created, err)
	}
	other, created, err := s.Propose(ctx, detectorProposal(nick, ClassSecret))
	if err != nil || !created || other.Status != StatusProposed || other.Class != ClassSecret {
		t.Fatalf("a different class reopens: %+v %v %v", other, created, err)
	}
}

func TestStore_Invalid(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	email := resolve(t, ctx, s, sch, "email")
	if _, err := s.ResolveColumn(ctx, sch, "users", "nope"); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("unknown column: %v", err)
	}
	if _, err := s.ResolveColumn(ctx, "pg_catalog", "pg_class", "relname"); !errors.Is(err,
		ErrColumnNotFound) {
		t.Errorf("system catalogs are not classified: %v", err)
	}
	if _, err := s.ResolveColumn(ctx, "sage", "users", "password"); !errors.Is(err,
		ErrColumnNotFound) {
		t.Errorf("pg_sage's own schema is not classified: %v", err)
	}
	if _, err := s.Set(ctx, email, Class("public"), "a@example.com", ""); !errors.Is(err,
		ErrInvalidClass) {
		t.Errorf("bad class: %v", err)
	}
	if _, err := s.Set(ctx, email, ClassPII, "", ""); !errors.Is(err, ErrInvalidActor) {
		t.Errorf("blank actor: %v", err)
	}
	ghost := email
	ghost.AttNum = 99
	if _, err := s.Set(ctx, ghost, ClassPII, "a@example.com", ""); !errors.Is(err,
		ErrColumnNotFound) {
		t.Errorf("non-existent attnum: %v", err)
	}
	table := Column{RelID: email.RelID}
	if _, err := s.Set(ctx, table, ClassPII, "a@example.com", ""); !errors.Is(err,
		ErrInvalidClass) {
		t.Errorf("table-level classes are untrusted_input only: %v", err)
	}
	if got, err := s.Set(ctx, table, ClassUntrusted, "a@example.com", ""); err != nil ||
		got.Column.AttNum != 0 || got.Column.Table != "users" {
		t.Errorf("table-level untrusted_input: %+v %v", got, err)
	}
	noEvidence := detectorProposal(email, ClassPII)
	noEvidence.Evidence = nil
	if _, _, err := s.Propose(ctx, noEvidence); !errors.Is(err, ErrNoEvidence) {
		t.Errorf("detector proposal without evidence: %v", err)
	}
	if _, _, err := s.Propose(ctx, detectorProposal(email, ClassClean)); !errors.Is(err,
		ErrInvalidClass) {
		t.Errorf("only an operator classifies clean: %v", err)
	}
	if _, err := NewStore(nil).ClassOf(ctx, 1, 1); !errors.Is(err, ErrNoStore) {
		t.Errorf("nil pool: %v", err)
	}
}

func TestStore_ListPagesAndFilters(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	for _, name := range []string{"email", "password", "bio"} {
		if _, _, err := s.Propose(ctx, detectorProposal(resolve(t, ctx, s, sch, name),
			ClassPII)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Set(ctx, resolve(t, ctx, s, sch, "nickname"), ClassClean, "a@example.com",
		""); err != nil {
		t.Fatal(err)
	}
	page, next, err := s.List(ctx, Filter{Limit: 2})
	if err != nil || len(page) != 2 || next == 0 {
		t.Fatalf("page 1: %d %d %v", len(page), next, err)
	}
	rest, next2, err := s.List(ctx, Filter{Limit: 2, After: next})
	if err != nil || len(rest) != 2 || next2 != 0 {
		t.Fatalf("page 2: %d %d %v", len(rest), next2, err)
	}
	if page[0].ID == rest[0].ID || page[1].Column.Schema != sch {
		t.Fatalf("pages overlap or lack live names: %+v %+v", page, rest)
	}
	only, _, err := s.List(ctx, Filter{Status: []Status{StatusConfirmed}})
	if err != nil || len(only) != 1 || only[0].Column.Name != "nickname" {
		t.Fatalf("status filter: %+v %v", only, err)
	}
	if _, _, err := s.List(ctx, Filter{Status: []Status{"maybe"}}); !errors.Is(err,
		ErrInvalidStatus) {
		t.Fatalf("bad status filter: %v", err)
	}
}

func TestStore_ConcurrentProposals(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	email := resolve(t, ctx, s, sch, "email")
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			class := ClassPII
			if i%3 == 0 {
				class = ClassSecret
			}
			_, c, err := s.Propose(ctx, detectorProposal(email, class))
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			if c {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.facts
		WHERE fact_type = 'column_class' AND subject_relid = $1 AND subject_attnum = $2`,
		email.RelID, email.AttNum).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows for one column: %d %v", rows, err)
	}
	if created < 1 {
		t.Fatal("no racer created the proposal")
	}
	// A pending proposal only ever narrows: secret wins whatever the order.
	if eff, err := s.ClassOf(ctx, email.RelID, email.AttNum); err != nil ||
		eff.Class != ClassSecret {
		t.Fatalf("final pending class: %+v %v", eff, err)
	}
}
