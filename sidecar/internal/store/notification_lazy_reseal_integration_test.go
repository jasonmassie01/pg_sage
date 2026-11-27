//go:build integration

package store

import (
	"fmt"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// security-08 review: the lazy re-seal on read (the only re-seal YAML fleet
// mode gets) must open values sealed under the previous key, and must never
// overwrite a channel edited between its read and its write.

func TestChannelSecrets_LazyResealRotatesPreviousKeyOnRead(t *testing.T) {
	pool, ctx := requireDB(t)
	truncateChannels(t, pool)
	storeA := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyA)
	id := insertRawChannel(t, pool, uniqueName("s08-lazy"), `{"routing_key":"rk-lazy"}`)
	if _, err := storeA.GetChannel(ctx, id); err != nil {
		t.Fatalf("seal under A: %v", err)
	}
	kid, _ := crypto.SealedKeyID(rawRoutingKey(t, pool, id))
	if kid != crypto.KeyIDFor(rotationKeyA) {
		t.Fatalf("setup: key id = %q, want key A", kid)
	}
	storeB := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyB).
		WithPreviousSecretKey(rotationKeyA)
	ch, err := storeB.GetChannel(ctx, id)
	if err != nil || ch.Config["routing_key"] != "rk-lazy" {
		t.Fatalf("GetChannel with previous key = %v, %v", ch, err)
	}
	kid, _ = crypto.SealedKeyID(rawRoutingKey(t, pool, id))
	if kid != crypto.KeyIDFor(rotationKeyB) {
		t.Fatalf("after lazy reseal key id = %q, want key B", kid)
	}
	onlyB := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyB)
	if ch, err := onlyB.GetChannel(ctx, id); err != nil || ch.Config["routing_key"] != "rk-lazy" {
		t.Fatalf("key B alone after lazy reseal = %v, %v", ch, err)
	}
}

// Concurrency: lazy re-seals racing UpdateChannel never resurrect an older
// value; the last update always survives, sealed under the active key.
func TestChannelSecrets_LazyResealRacingUpdatesNeverLosesAWrite(t *testing.T) {
	pool, ctx := requireDB(t)
	truncateChannels(t, pool)
	s := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyB)
	for round := 0; round < 15; round++ {
		id := insertRawChannel(t, pool, uniqueName("s08-race"),
			fmt.Sprintf(`{"routing_key":"rk-old-%d"}`, round))
		want := fmt.Sprintf("rk-new-%d", round)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.GetChannel(ctx, id)
				errs <- err
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.UpdateChannel(ctx, id, uniqueName("s08-race-upd"),
				map[string]string{"routing_key": want}, true)
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent op: %v", round, err)
			}
		}
		ch, err := s.GetChannel(ctx, id)
		if err != nil || ch.Config["routing_key"] != want {
			t.Fatalf("round %d: final routing_key = %v, %v; want %s (update lost)",
				round, ch, err, want)
		}
		if raw := rawRoutingKey(t, pool, id); !crypto.IsSealed(raw) {
			t.Fatalf("round %d: final value is not sealed: %.20q", round, raw)
		}
	}
}
