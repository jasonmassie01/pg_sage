package managedparam

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/schema"
)

func unresolvedProposal(t *testing.T, parameter, value string) Proposal {
	t.Helper()
	p, err := Build(intent(t, "rds", parameter, value),
		Target{Provider: "rds", Unresolved: "throttled"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Boundary: a rejection 1 s inside the cooldown still holds; 1 s past it,
// the change is proposed again. A placeholder build of the same change is
// held by the same rejection.
func TestStoreRejectionCooldownBoundary(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rec, _, err := s.Upsert(ctx, buildProposal(t, "work_mem", "96MB"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, rec.ID, false, 3, "no"); err != nil {
		t.Fatal(err)
	}
	setDecided := func(interval string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE sage.managed_change_proposals
			SET decided_at = now() - $2::interval WHERE id = $1`, rec.ID,
			interval); err != nil {
			t.Fatal(err)
		}
	}
	setDecided("7 days - 1 second")
	for name, p := range map[string]Proposal{"resolved": buildProposal(t, "work_mem", "96MB"),
		"placeholder": unresolvedProposal(t, "work_mem", "96MB")} {
		got, created, err := s.Upsert(ctx, p, 1)
		if err != nil || created || got.ID != rec.ID || got.Status != StatusRejected {
			t.Fatalf("%s inside the cooldown = %+v %t %v, want the rejection", name, got,
				created, err)
		}
	}
	setDecided("7 days + 1 second")
	got, created, err := s.Upsert(ctx, buildProposal(t, "work_mem", "96MB"), 1)
	if err != nil || !created || got.ID == rec.ID || got.Status != StatusPending {
		t.Fatalf("past the cooldown = %+v %t %v, want a new pending proposal", got, created,
			err)
	}
}

// A placeholder build never replaces a resolved proposal; a resolved
// build replaces a placeholder one and its target column.
func TestStoreRefreshKeepsResolvedTarget(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	resolved, _, err := s.Upsert(ctx, buildProposal(t, "work_mem", "96MB"), 1)
	if err != nil {
		t.Fatal(err)
	}
	again, created, err := s.Upsert(ctx, unresolvedProposal(t, "work_mem", "96MB"), 1)
	if err != nil || created || again.ID != resolved.ID {
		t.Fatalf("placeholder upsert = %+v %t %v, want a refresh of %d", again, created, err,
			resolved.ID)
	}
	if again.Proposal.CLI != resolved.Proposal.CLI ||
		again.UpdatedAt.Before(resolved.UpdatedAt) ||
		storedTarget(t, pool, again.ID) != "orders-pg16" {
		t.Fatalf("placeholder overwrote the resolved proposal: %+v", again.Proposal)
	}
	other, _, err := s.Upsert(ctx, unresolvedProposal(t, "shared_buffers", "2GB"), 2)
	if err != nil || storedTarget(t, pool, other.ID) != "<db-parameter-group>" {
		t.Fatalf("placeholder insert = %+v, %v", other, err)
	}
	fixed, created, err := s.Upsert(ctx, buildProposal(t, "shared_buffers", "2GB"), 2)
	if err != nil || created || fixed.ID != other.ID ||
		storedTarget(t, pool, fixed.ID) != "orders-pg16" ||
		!strings.Contains(fixed.Proposal.CLI, "orders-pg16") {
		t.Fatalf("resolved refresh = %+v %t %v", fixed, created, err)
	}
}

// Concurrency: simultaneous upserts of one new change insert it once and
// every caller gets that record, never a unique-index violation.
func TestStoreConcurrentUpsertSameChange(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	p := buildProposal(t, "work_mem", "96MB")
	type result struct {
		id      int64
		created bool
		err     error
	}
	out := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, created, err := s.Upsert(ctx, p, 1)
			out <- result{rec.ID, created, err}
		}()
	}
	wg.Wait()
	close(out)
	ids, created := map[int64]bool{}, 0
	for r := range out {
		if r.err != nil {
			t.Fatalf("concurrent upsert: %v", r.err)
		}
		ids[r.id] = true
		if r.created {
			created++
		}
	}
	if len(ids) != 1 || created != 1 {
		t.Fatalf("ids=%v created=%d, want one record created once", ids, created)
	}
}

// Integration: the schema migration computes in SQL exactly the
// fingerprint Build computes in Go, in the column and in the proposal.
func TestFingerprintMigrationMatchesBuild(t *testing.T) {
	pool, ctx := testPool(t)
	p := buildProposal(t, "work_mem", "96MB")
	legacy := p
	legacy.Fingerprint = "0123456789abcdef"
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.managed_change_proposals
		(fingerprint, provider, mechanism, target, parameter, value, reboot_required,
		 proposal, status, decided_by, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'rejected', 1, now()) RETURNING id`,
		legacy.Fingerprint, p.Provider, p.Mechanism, p.Target, p.Parameter, p.Value,
		p.RebootRequired, raw).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var column, inProposal string
	if err := pool.QueryRow(ctx, `SELECT fingerprint, proposal->>'fingerprint'
		FROM sage.managed_change_proposals WHERE id = $1`, id).Scan(&column,
		&inProposal); err != nil {
		t.Fatal(err)
	}
	if column != p.Fingerprint || inProposal != p.Fingerprint {
		t.Fatalf("migrated fingerprint %q / %q, Build gives %q", column, inProposal,
			p.Fingerprint)
	}
	rec, created, err := NewStore(pool).Upsert(context.Background(), p, 1)
	if err != nil || created || rec.ID != id {
		t.Fatalf("the migrated rejection must hold the cooldown: %+v %t %v", rec, created, err)
	}
}
