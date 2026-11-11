package facts

import "testing"

// facts-09: a fact stored before repo and path were retired keeps loading
// with its value as stored; only new proposals are refused.
func TestStoredRetiredKeysStillLoad(t *testing.T) {
	pool, ctx := livePool(t)
	execAll(t, ctx, pool, `INSERT INTO sage.facts (fact_type, subject_kind, subject, value,
		source, proposed_by, evidence, status, decided_by, decided_at)
		VALUES ('owned_by_app_migrations', 'schema', 'legacy_app',
		'{"repo": "github.com/acme/app", "path": "db/migrate", "note": "old"}',
		'operator', 'alice@example.com', '[]', 'confirmed', 'alice@example.com', now())`)
	s := NewStore(pool)
	list, err := s.List(ctx, Filter{Type: TypeAppMigrations})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	f := list[0]
	if f.Value["repo"] != "github.com/acme/app" || f.Value["path"] != "db/migrate" ||
		f.Value["note"] != "old" || f.Status != StatusConfirmed {
		t.Fatalf("stored fact = %+v", f)
	}
	confirmed, err := s.Confirmed(ctx)
	if err != nil || len(confirmed) != 1 || confirmed[0].ID != f.ID {
		t.Fatalf("the stored fact stopped binding: %+v %v", confirmed, err)
	}
	if _, _, err := s.Propose(ctx, Proposal{Type: TypeAppMigrations, Kind: KindSchema,
		Subject: "legacy_app", Source: SourceOperator, ProposedBy: "bob@example.com",
		Value: map[string]string{"repo": "github.com/acme/app"}}); err == nil {
		t.Fatal("a new proposal with repo was accepted")
	}
	got, err := s.Get(ctx, f.ID)
	if err != nil || got.Proposals != 1 || got.Value["repo"] != "github.com/acme/app" {
		t.Fatalf("a refused proposal changed the stored fact: %+v %v", got, err)
	}
}
