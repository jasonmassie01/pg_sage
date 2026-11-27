package clone

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migration-07: a crash between create and destroy leaves a ready receipt;
// once it expires the reaper destroys its clone. It never touches another
// deployment's, adapter's, scope's or purpose's receipts, and never
// destroys a receipt that names no provider resource.

var reaperScope = ReceiptScope{DeploymentID: testDeployment, Adapter: "dle",
	Scope: "https://dle.invalid"}

const otherDeployment = "00000000-0000-4000-8000-0000000000ff"

type seeded struct {
	scope   ReceiptScope
	purpose string
	ref     string // "" leaves the receipt creating
	expired bool
	status  string // a status to force after ready (destroying, destroyed)
}

func seedReceipt(t *testing.T, s *ReceiptStore, pool *pgxpool.Pool, name string,
	sd seeded) Receipt {
	t.Helper()
	ctx := context.Background()
	r, err := s.Record(ctx, Receipt{DeploymentID: sd.scope.DeploymentID,
		Adapter: sd.scope.Adapter, Scope: sd.scope.Scope, Name: name, Purpose: sd.purpose,
		ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if sd.ref != "" {
		if r, err = s.MarkReady(ctx, r.ID, sd.ref); err != nil {
			t.Fatalf("seed %s ready: %v", name, err)
		}
	}
	if sd.status != "" {
		if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances SET status = $2
			WHERE id = $1`, r.ID, sd.status); err != nil {
			t.Fatal(err)
		}
	}
	if sd.expired {
		if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances
			SET expires_at = now() - interval '1 minute' WHERE id = $1`, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	return receiptByID(t, pool, r.ID)
}

// reaperRows is one receipt per case the reaper must tell apart.
func reaperRows() map[string]seeded {
	other := func(mut func(*ReceiptScope)) ReceiptScope {
		sc := reaperScope
		mut(&sc)
		return sc
	}
	return map[string]seeded{
		"crashed": {reaperScope, PurposeRehearsal, "c-crashed", true, ""},
		"retry":   {reaperScope, PurposeRehearsal, "c-retry", true, StatusDestroying},
		"in-use":  {reaperScope, PurposeRehearsal, "c-in-use", false, ""},
		"no-ref":  {reaperScope, PurposeRehearsal, "", true, ""},
		"closed":  {reaperScope, PurposeRehearsal, "c-closed", true, StatusDestroyed},
		"sandbox": {reaperScope, PurposeSandbox, "c-sandbox", true, ""},
		"other-deploy": {other(func(s *ReceiptScope) { s.DeploymentID = otherDeployment }),
			PurposeRehearsal, "c-od", true, ""},
		"other-adapter": {other(func(s *ReceiptScope) { s.Adapter = "neon" }),
			PurposeRehearsal, "c-oa", true, ""},
		"other-scope": {other(func(s *ReceiptScope) { s.Scope = "https://dle2.invalid" }),
			PurposeRehearsal, "c-os", true, ""},
	}
}

func TestReaper_DestroysOnlyItsOwnExpiredReceipts(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	rows := reaperRows()
	seededIDs := map[string]int64{}
	for name, sd := range rows {
		seededIDs[name] = seedReceipt(t, s, pool, name, sd).ID
	}
	inner := &fakeInner{pool: pool}
	report, err := Reaper{Store: s, Provider: inner, Scope: reaperScope,
		Purpose: PurposeRehearsal}.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(inner.destroyed)
	if len(inner.destroyed) != 2 || inner.destroyed[0] != "c-crashed" ||
		inner.destroyed[1] != "c-retry" {
		t.Fatalf("destroyed %v, want [c-crashed c-retry]", inner.destroyed)
	}
	if len(report.Destroyed) != 2 || len(report.Failed) != 0 {
		t.Fatalf("report %+v", report)
	}
	if len(report.Unresolved) != 1 || report.Unresolved[0].ID != seededIDs["no-ref"] {
		t.Fatalf("unresolved %+v, want only the receipt without a ref", report.Unresolved)
	}
	want := map[string]string{"crashed": StatusDestroyed, "retry": StatusDestroyed,
		"in-use": StatusReady, "no-ref": StatusCreating, "closed": StatusDestroyed,
		"sandbox": StatusReady, "other-deploy": StatusReady, "other-adapter": StatusReady,
		"other-scope": StatusReady}
	for name, status := range want {
		if got := receiptByID(t, pool, seededIDs[name]); got.Status != status {
			t.Errorf("%s: status %s, want %s", name, got.Status, status)
		}
	}
}

func TestReaper_FailedTeardownIsRetried(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	r := seedReceipt(t, s, pool, "flaky", seeded{reaperScope, PurposeRehearsal,
		"c-flaky", true, ""})
	inner := &fakeInner{pool: pool, destroyErr: errors.New("dle: 502")}
	reaper := Reaper{Store: s, Provider: inner, Scope: reaperScope,
		Purpose: PurposeRehearsal}
	report, err := reaper.Reap(ctx)
	if err != nil || report.Failed[r.ID] == nil || len(report.Destroyed) != 0 {
		t.Fatalf("first pass: %+v %v", report, err)
	}
	if got := receiptByID(t, pool, r.ID); got.Status != StatusDestroying {
		t.Fatalf("after a failed teardown: %s, want destroying", got.Status)
	}
	inner.destroyErr = nil
	report, err = reaper.Reap(ctx)
	if err != nil || len(report.Destroyed) != 1 || report.Destroyed[0].Status !=
		StatusDestroyed {
		t.Fatalf("second pass: %+v %v", report, err)
	}
	if report, err = reaper.Reap(ctx); err != nil || len(report.Destroyed) != 0 {
		t.Fatalf("a destroyed receipt is reaped again: %+v %v", report, err)
	}
}

func TestReaper_InvalidAndEmpty(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	inner := &fakeInner{pool: pool}
	report, err := Reaper{Store: s, Provider: inner, Scope: reaperScope,
		Purpose: PurposeRehearsal}.Reap(ctx)
	if err != nil || len(report.Destroyed)+len(report.Unresolved)+len(report.Failed) != 0 {
		t.Fatalf("empty table: %+v %v", report, err)
	}
	bad := reaperScope
	bad.DeploymentID = "nope"
	if _, err := (Reaper{Store: s, Provider: inner, Scope: bad,
		Purpose: PurposeRehearsal}).Reap(ctx); !errors.Is(err, ErrInvalidReceipt) {
		t.Errorf("bad deployment: %v", err)
	}
	if _, err := (Reaper{Store: s, Provider: inner, Scope: ReceiptScope{
		DeploymentID: testDeployment}, Purpose: PurposeRehearsal}).Reap(ctx); !errors.Is(err,
		ErrInvalidReceipt) {
		t.Errorf("blank adapter and scope: %v", err)
	}
	if _, err := (Reaper{Store: s, Provider: inner, Scope: reaperScope,
		Purpose: "fun"}).Reap(ctx); !errors.Is(err, ErrInvalidReceipt) {
		t.Errorf("bad purpose: %v", err)
	}
	if _, err := (Reaper{Store: NewReceiptStore(nil), Provider: inner, Scope: reaperScope,
		Purpose: PurposeRehearsal}).Reap(ctx); !errors.Is(err, ErrNoReceiptStore) {
		t.Errorf("nil pool: %v", err)
	}
	if _, err := (Reaper{Store: s, Scope: reaperScope,
		Purpose: PurposeRehearsal}).Reap(ctx); err == nil {
		t.Error("nil provider accepted")
	}
}

func TestReceiptStore_ReapableIsBounded(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.clone_instances (deployment_id, adapter,
		scope, ref, name, purpose, status, expires_at)
		SELECT $1::uuid, 'dle', 'https://dle.invalid', 'c-' || g, 'bulk-' || g,
		       'rehearsal', 'ready', now() - interval '1 hour' + g * interval '1 second'
		FROM generate_series(1, $2::int) g`, testDeployment, MaxExpiredPerPass+5); err != nil {
		t.Fatal(err)
	}
	got, err := s.Reapable(ctx, ReapQuery{Scope: reaperScope, Purpose: PurposeRehearsal,
		StaleAfter: DefaultHolderStaleAfter})
	if err != nil || len(got) != MaxExpiredPerPass {
		t.Fatalf("reapable: %d receipts, %v; want %d", len(got), err, MaxExpiredPerPass)
	}
	if got[0].Ref != "c-1" || got[len(got)-1].Ref != "c-100" {
		t.Fatalf("not the oldest first: %s .. %s", got[0].Ref, got[len(got)-1].Ref)
	}
}
