package upgradecheck

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// drill-02b review: LiveRehearsalReceipt is checked against receipts a real
// ReceiptingProvider writes. The clone's resource id is the receipt's ref,
// never its name (rehearsal-<uuid>), so a lookup by name never matched an
// Aurora or RDS clone and every AWS rehearsal was refused.

// No concurrent access tests: LiveRehearsalReceipt is one read of the store.

func receiptedClone(t *testing.T, db liveDB, purpose string) (*clone.ReceiptingProvider,
	clone.Clone, string) {
	t.Helper()
	store := clone.NewReceiptStore(db.pool)
	p, err := clone.NewReceiptingProvider(&fixtureInner{dsn: "postgres://unused/app"}, store,
		clone.ReceiptingConfig{Adapter: "fixture", Scope: "drill-02b-live-" + purpose,
			Deployment: func(context.Context) (string, error) { return rehearsalDeployment, nil },
			Purpose:    purpose, TTL: time.Hour, Lifetime: db.ctx, MaxCreatesPerHour: 60})
	if err != nil {
		t.Fatalf("receipting provider: %v", err)
	}
	cl, err := p.Create(db.ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatalf("create the receipted clone: %v", err)
	}
	id, ok := p.ReceiptOf(cl.ID)
	if !ok {
		t.Fatalf("no receipt for clone %s", cl.ID)
	}
	var name string
	if err := db.pool.QueryRow(db.ctx, "SELECT name FROM sage.clone_instances WHERE id = $1",
		id).Scan(&name); err != nil {
		t.Fatalf("read the receipt name: %v", err)
	}
	if name == cl.ID {
		t.Fatalf("receipt name %q equals the clone id; the test would not tell ref from name",
			name)
	}
	return p, cl, name
}

func TestLiveRehearsalReceiptDB_MatchesTheClonesResourceID(t *testing.T) {
	db := bootstrapped(t)
	p, cl, name := receiptedClone(t, db, clone.PurposeRehearsal)
	live := LiveRehearsalReceipt(clone.NewReceiptStore(db.pool))
	if ok, err := live(db.ctx, cl.ID); err != nil || !ok {
		t.Fatalf("Live(%s) = %v, %v; want the receipted rehearsal clone live", cl.ID, ok, err)
	}
	if ok, err := live(db.ctx, name); err != nil || ok {
		t.Fatalf("Live(receipt name %s) = %v, %v; a receipt name is no resource id", name,
			ok, err)
	}
	if ok, err := live(db.ctx, ""); err != nil || ok {
		t.Fatalf("Live(\"\") = %v, %v", ok, err)
	}
	if err := p.Destroy(db.ctx, cl); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if ok, err := live(db.ctx, cl.ID); err != nil || ok {
		t.Fatalf("Live after destroy = %v, %v; want no live receipt", ok, err)
	}
}

func TestLiveRehearsalReceiptDB_OtherPurposeIsNotARehearsal(t *testing.T) {
	db := bootstrapped(t)
	_, cl, _ := receiptedClone(t, db, clone.PurposeSandbox)
	live := LiveRehearsalReceipt(clone.NewReceiptStore(db.pool))
	if ok, err := live(db.ctx, cl.ID); err != nil || ok {
		t.Fatalf("Live(sandbox clone %s) = %v, %v; want refused", cl.ID, ok, err)
	}
}

func TestLiveRehearsalReceiptDB_ExpiredReceiptIsNotLive(t *testing.T) {
	db := bootstrapped(t)
	_, cl, _ := receiptedClone(t, db, clone.PurposeRehearsal)
	if _, err := db.pool.Exec(db.ctx, `UPDATE sage.clone_instances
		SET expires_at = now() - interval '1 second' WHERE ref = $1`, cl.ID); err != nil {
		t.Fatalf("expire the receipt: %v", err)
	}
	live := LiveRehearsalReceipt(clone.NewReceiptStore(db.pool))
	if ok, err := live(db.ctx, cl.ID); err != nil || ok {
		t.Fatalf("Live(expired %s) = %v, %v; want not live", cl.ID, ok, err)
	}
}

// The AWS upgrader over the real receipt check upgrades the receipted
// clone and nothing else.
func TestAWSUpgraderDB_UpgradesAReceiptedRehearsalClone(t *testing.T) {
	db := bootstrapped(t)
	_, cl, _ := receiptedClone(t, db, clone.PurposeRehearsal)
	api := aurora14()
	u := auroraUpgrader(api)
	u.Live = LiveRehearsalReceipt(clone.NewReceiptStore(db.pool))
	got, err := u.Upgrade(db.ctx, cl, 17)
	if err != nil {
		t.Fatalf("Upgrade of the receipted clone %s: %v", cl.ID, err)
	}
	if got.ID != cl.ID || len(api.modified) != 1 || api.modified[0] != cl.ID+"=17.10" {
		t.Fatalf("Upgrade = %+v, modified %v; want %s to 17.10", got, api.modified, cl.ID)
	}
}

func TestLiveRehearsalReceipt_NilStoreFailsClosed(t *testing.T) {
	ok, err := LiveRehearsalReceipt(nil)(context.Background(), "sage-rehearsal-0a1b2c3d")
	if ok || !errors.Is(err, clone.ErrNoReceiptStore) {
		t.Fatalf("Live over a nil store = %v, %v; want ErrNoReceiptStore", ok, err)
	}
	ok, err = LiveRehearsalReceipt(clone.NewReceiptStore(nil))(context.Background(), "x")
	if ok || !errors.Is(err, clone.ErrNoReceiptStore) {
		t.Fatalf("Live over a store without a pool = %v, %v", ok, err)
	}
}
