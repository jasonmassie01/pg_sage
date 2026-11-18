package clone

import (
	"context"
	"errors"
	"testing"
)

// drill-02b: ActiveRef finds a ready, unexpired receipt by the provider
// resource id MarkReady recorded, not by the receipt's name.

func TestReceipt_ActiveRefMatchesTheResourceIDOnly(t *testing.T) {
	s, pool, ctx := receiptStore(t)
	r, err := s.Record(ctx, validReceipt("rehearsal-name"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ActiveRef(ctx, "sage-rehearsal-0a1b2c3d"); err != nil || ok {
		t.Fatalf("creating receipt found by ref: %v %v", ok, err)
	}
	if _, err := s.MarkReady(ctx, r.ID, "sage-rehearsal-0a1b2c3d"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ActiveRef(ctx, "sage-rehearsal-0a1b2c3d")
	if err != nil || !ok || got.ID != r.ID || got.Ref != "sage-rehearsal-0a1b2c3d" {
		t.Fatalf("ready receipt by ref: %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.ActiveRef(ctx, "rehearsal-name"); err != nil || ok {
		t.Fatalf("receipt name matched as a ref: %v %v", ok, err)
	}
	if _, ok, err := s.ActiveRef(ctx, "  "); err != nil || ok {
		t.Fatalf("blank ref: %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances
		SET expires_at = now() - interval '1 second' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ActiveRef(ctx, "sage-rehearsal-0a1b2c3d"); err != nil || ok {
		t.Fatalf("expired receipt by ref: %v %v", ok, err)
	}
}

func TestReceipt_ActiveRefNilPool(t *testing.T) {
	_, _, err := NewReceiptStore(nil).ActiveRef(context.Background(), "x")
	if !errors.Is(err, ErrNoReceiptStore) {
		t.Fatalf("nil pool ActiveRef: %v", err)
	}
}
