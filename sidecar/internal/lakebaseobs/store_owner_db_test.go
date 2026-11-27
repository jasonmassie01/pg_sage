package lakebaseobs

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// No concurrent writer tests here beyond the lock holder below: cleanupOwner
// keeps one transaction, and the policy fence races are pinned in store_db_test.go.

type ownerFixture struct {
	s        *Store
	base     Scope
	scopes   []Scope
	bindings []string
	events   []Event
	now      time.Time
}

// newOwnerFixture binds three scopes of one owner and commits one payload
// event for each; the third scope is the one a later policy drops.
func newOwnerFixture(t *testing.T) *ownerFixture {
	t.Helper()
	s, _ := dbStore(t)
	ctx := context.Background()
	f := &ownerFixture{s: s, base: testScope(), now: time.Now().UTC()}
	f.base.DeploymentID = uuid.NewString()
	f.base.DatabaseID = uuid.NewString()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, b := range f.bindings {
			_, _ = s.pool.Exec(c, "DELETE FROM sage.lakebaseobs_inbox WHERE binding=$1", b)
		}
		for _, table := range []string{"lakebaseobs_owners", "lakebaseobs_owner_policy"} {
			_, _ = s.pool.Exec(c, "DELETE FROM sage."+table+" WHERE deployment_id=$1::uuid",
				f.base.DeploymentID)
		}
	})
	for _, branch := range []string{"br-keep-a", "br-keep-b", "br-stale"} {
		sc := f.base
		sc.BranchID = branch
		if err := s.BindOwner(ctx, sc); err != nil {
			t.Fatal(err)
		}
		fields := testFields(Plans, f.now.Add(-time.Hour))
		fields["branch_id"] = branch
		e, err := NewEvent(Plans, sc, fields, f.now, true)
		if err != nil {
			t.Fatal(err)
		}
		b := sc.Hash() + ":v1:payload"
		if err = s.CommitBatch(ctx, b, Plans, Cursor{}, Cursor{}, []Event{e},
			SourceRetention); err != nil {
			t.Fatal(err)
		}
		f.scopes = append(f.scopes, sc)
		f.bindings = append(f.bindings, b)
		f.events = append(f.events, e)
	}
	return f
}

func (f *ownerFixture) hashes(idx ...int) []string {
	out := []string{}
	for _, i := range idx {
		out = append(out, f.scopes[i].Hash())
	}
	return out
}

// rowState reports, per binding, whether the inbox row exists and whether it
// still holds its event document.
func (f *ownerFixture) rowState(t *testing.T) (present, payload []bool) {
	t.Helper()
	for _, b := range f.bindings {
		var rows, held int
		err := f.s.pool.QueryRow(context.Background(), `SELECT count(*),
count(*) FILTER (WHERE event IS NOT NULL) FROM sage.lakebaseobs_inbox WHERE binding=$1`,
			b).Scan(&rows, &held)
		if err != nil {
			t.Fatal(err)
		}
		present = append(present, rows == 1)
		payload = append(payload, held == 1)
	}
	return present, payload
}

func (f *ownerFixture) policy(t *testing.T) ([]string, bool) {
	t.Helper()
	var allowed []string
	var payload bool
	err := f.s.pool.QueryRow(context.Background(), `SELECT allowed_scopes,allow_payload
FROM sage.lakebaseobs_owner_policy WHERE deployment_id=$1::uuid AND database_id=$2::uuid`,
		f.base.DeploymentID, f.base.DatabaseID).Scan(&allowed, &payload)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(allowed)
	return allowed, payload
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func TestDBCleanupOwnerScopesErasesOnlyTheStaleScope(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := context.Background()
	if err := f.s.Ack(ctx, f.bindings[2], f.events[2].ID, "sink.v1"); err != nil {
		t.Fatal(err)
	}
	keep := f.hashes(0, 1)
	if err := f.s.CleanupOwnerScopes(ctx, f.base.DeploymentID, f.base.DatabaseID, keep, true,
		SourceRetention, f.now); err != nil {
		t.Fatalf("CleanupOwnerScopes: %v", err)
	}
	present, payload := f.rowState(t)
	if !slices.Equal(present, []bool{true, true, true}) {
		t.Fatalf("rows present %v, want every identity kept for replay protection", present)
	}
	if !slices.Equal(payload, []bool{true, true, false}) {
		t.Fatalf("payload held %v, want only the stale scope erased", payload)
	}
	allowed, allowPayload := f.policy(t)
	if !slices.Equal(allowed, sorted(keep)) || !allowPayload {
		t.Fatalf("policy %v payload=%v, want %v payload=true", allowed, allowPayload, sorted(keep))
	}
	var acks int
	if err := f.s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_acks WHERE binding=$1",
		f.bindings[2]).Scan(&acks); err != nil || acks != 1 {
		t.Fatalf("acks for the stale scope = %d (%v), want 1 kept", acks, err)
	}
	owned, err := f.s.OwnerScopes(ctx, f.base.DeploymentID, f.base.DatabaseID)
	if err != nil || len(owned) != 3 {
		t.Fatalf("owner rows %v (%v), want all 3 kept for a later cleanup", owned, err)
	}
}

func TestDBCleanupOwnerScopesWithNoAllowedScopeDisablesTheSource(t *testing.T) {
	for name, allowed := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			f := newOwnerFixture(t)
			err := f.s.CleanupOwnerScopes(context.Background(), f.base.DeploymentID,
				f.base.DatabaseID, allowed, true, SourceRetention, f.now)
			if err != nil {
				t.Fatalf("CleanupOwnerScopes: %v", err)
			}
			present, payload := f.rowState(t)
			if !slices.Equal(present, []bool{true, true, true}) ||
				!slices.Equal(payload, []bool{false, false, false}) {
				t.Fatalf("present %v payload %v, want identities kept and payloads erased",
					present, payload)
			}
			got, _ := f.policy(t)
			if got == nil || len(got) != 0 {
				t.Fatalf("allowed scopes %#v, want an empty non-null array", got)
			}
		})
	}
}

func TestDBPolicyOwnerScopesWithNoRowsIsEmptyNotNil(t *testing.T) {
	s, _ := dbStore(t)
	scopes, err := s.PolicyOwnerScopes(context.Background(), uuid.NewString(), uuid.NewString(),
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("PolicyOwnerScopes: %v", err)
	}
	if scopes == nil || len(scopes) != 0 {
		t.Fatalf("scopes %#v, want an empty non-nil slice", scopes)
	}
}

func TestDBCleanupOwnerPolicyKeepsTheBindingScopesAndAddsNone(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := context.Background()
	binding := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if _, err := f.s.pool.Exec(ctx, `UPDATE sage.lakebaseobs_owners SET policy_binding=$1
WHERE deployment_id=$2::uuid AND scope_hash=ANY($3::text[])`, binding, f.base.DeploymentID,
		f.hashes(0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CleanupOwnerPolicy(ctx, f.base.DeploymentID, f.base.DatabaseID, binding, false,
		SourceRetention, f.now); err != nil {
		t.Fatalf("CleanupOwnerPolicy: %v", err)
	}
	allowed, allowPayload := f.policy(t)
	if !slices.Equal(allowed, sorted(f.hashes(0, 1))) || allowPayload {
		t.Fatalf("policy %v payload=%v, want the 2 bound scopes, payload off",
			allowed, allowPayload)
	}
	// Payload is now revoked, so every payload-enabled event loses its document.
	if _, payload := f.rowState(t); !slices.Equal(payload, []bool{false, false, false}) {
		t.Fatalf("payload held %v after payload revocation, want none", payload)
	}
}

// The identity delete keeps a row whose identity_until is a microsecond in the
// future and deletes one whose identity_until equals now.
func TestDBCleanupOwnerDeletesIdentitiesAtTheirExpiryBoundary(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := context.Background()
	now := f.now.Truncate(time.Microsecond)
	set := func(binding string, until time.Time) {
		t.Helper()
		if _, err := f.s.pool.Exec(ctx, `UPDATE sage.lakebaseobs_inbox SET identity_until=$2
WHERE binding=$1`, binding, until); err != nil {
			t.Fatal(err)
		}
	}
	set(f.bindings[0], now)
	set(f.bindings[1], now.Add(time.Microsecond))
	if err := f.s.CleanupOwnerScopes(ctx, f.base.DeploymentID, f.base.DatabaseID,
		f.hashes(0, 1, 2), true, SourceRetention, now); err != nil {
		t.Fatalf("CleanupOwnerScopes: %v", err)
	}
	present, payload := f.rowState(t)
	if !slices.Equal(present, []bool{false, true, true}) {
		t.Fatalf("rows present %v, want only the row expiring at now deleted", present)
	}
	if !slices.Equal(payload, []bool{false, true, true}) {
		t.Fatalf("payload held %v, want the allowed live rows untouched", payload)
	}
}

// waitForInboxLock waits until a cleanup statement on the inbox blocks on a
// row lock held by another transaction.
func waitForInboxLock(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	app := s.pool.Config().ConnConfig.RuntimeParams["application_name"]
	for {
		var waiting bool
		err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%SET event=NULL%'
AND pid<>pg_backend_pid())`, app).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("cleanup never blocked on the held inbox row lock")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestDBCleanupOwnerCancelledMidTransactionRollsBack(t *testing.T) {
	f := newOwnerFixture(t)
	ctx := context.Background()
	all := f.hashes(0, 1, 2)
	if err := f.s.CleanupOwnerScopes(ctx, f.base.DeploymentID, f.base.DatabaseID, all, true,
		SourceRetention, f.now); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	holder, err := f.s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err = holder.Exec(ctx, "SELECT 1 FROM sage.lakebaseobs_inbox WHERE binding=$1 FOR UPDATE",
		f.bindings[2]); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- f.s.CleanupOwnerScopes(cctx, f.base.DeploymentID, f.base.DatabaseID,
			f.hashes(0, 1), true, SourceRetention, f.now)
	}()
	// By now the policy row is already rewritten inside the cleanup transaction.
	waitForInboxLock(t, f.s)
	cancel()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled cleanup did not return")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup returned %v, want context.Canceled", err)
	}
	if err = holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	allowed, _ := f.policy(t)
	if !slices.Equal(allowed, sorted(all)) {
		t.Fatalf("policy %v after cancel, want the earlier %v (rolled back)",
			allowed, sorted(all))
	}
	if _, payload := f.rowState(t); !slices.Equal(payload, []bool{true, true, true}) {
		t.Fatalf("payload held %v after cancel, want nothing erased", payload)
	}
}
