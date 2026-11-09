//go:build integration

package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/crypto"
)

func insertRawChannel(t *testing.T, pool *pgxpool.Pool, name, cfgJSON string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.notification_channels
		(name, type, config) VALUES ($1, 'pagerduty', $2) RETURNING id`,
		name, cfgJSON).Scan(&id); err != nil {
		t.Fatalf("insert raw channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_channels WHERE id = $1`, id)
	})
	return id
}

func rawRoutingKey(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var raw string
	if err := pool.QueryRow(context.Background(), `SELECT config->>'routing_key'
		FROM sage.notification_channels WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read routing_key: %v", err)
	}
	return raw
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func TestChannelSecrets_SealedUnderChannelAAD(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyA)
	src := insertRawChannel(t, pool, uniqueName("s08-src"), `{"routing_key":"rk-src"}`)
	dst := insertRawChannel(t, pool, uniqueName("s08-dst"), `{"routing_key":"rk-dst"}`)
	for _, id := range []int{src, dst} {
		if _, err := ns.GetChannel(ctx, id); err != nil {
			t.Fatalf("GetChannel %d (lazy seal): %v", id, err)
		}
		if raw := rawRoutingKey(t, pool, id); !crypto.IsSealed(raw) {
			t.Fatalf("channel %d routing_key not sage-enc:v1: %.24q", id, raw)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.notification_channels SET config =
		(SELECT config FROM sage.notification_channels WHERE id = $1) WHERE id = $2`,
		src, dst); err != nil {
		t.Fatalf("copy config: %v", err)
	}
	if _, err := ns.GetChannel(ctx, dst); !errors.Is(err, crypto.ErrOpenFailed) {
		t.Fatalf("copied channel secret: err = %v, want ErrOpenFailed", err)
	}
	ch, err := ns.GetChannel(ctx, src)
	if err != nil || ch.Config["routing_key"] != "rk-src" {
		t.Fatalf("source channel = %v, %v", ch, err)
	}
}

// State transitions: plaintext, enc:v1 under A and sage-enc:v1 under A all
// end under B after one startup reseal; a second reseal has nothing to do.
func TestChannelSecrets_RotationReachesActiveKey(t *testing.T) {
	pool, ctx := requireDB(t)
	truncateChannels(t, pool)
	storeA := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyA)
	sealedA := insertRawChannel(t, pool, uniqueName("s08-sealed"), `{"routing_key":"rk-1"}`)
	if _, err := storeA.GetChannel(ctx, sealedA); err != nil {
		t.Fatalf("seal under A: %v", err)
	}
	ct, err := crypto.Encrypt("rk-2", rotationKeyA)
	if err != nil {
		t.Fatal(err)
	}
	encV1 := insertRawChannel(t, pool, uniqueName("s08-encv1"), fmt.Sprintf(
		`{"routing_key":"enc:v1:%s"}`, base64.StdEncoding.EncodeToString(ct)))
	plain := insertRawChannel(t, pool, uniqueName("s08-plain"), `{"routing_key":"rk-3"}`)

	storeB := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyB).
		WithPreviousSecretKey(rotationKeyA)
	report, err := storeB.ResealChannelSecrets(ctx)
	if err != nil {
		t.Fatalf("ResealChannelSecrets: %v", err)
	}
	if report.Sealed != 2 || report.Rotated != 1 || report.Unreadable != 0 {
		t.Fatalf("report = %+v, want sealed 2 (enc:v1 + plaintext), rotated 1", report)
	}
	onlyB := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyB)
	for id, want := range map[int]string{sealedA: "rk-1", encV1: "rk-2", plain: "rk-3"} {
		kid, err := crypto.SealedKeyID(rawRoutingKey(t, pool, id))
		if err != nil || kid != crypto.KeyIDFor(rotationKeyB) {
			t.Errorf("channel %d key id = %q, %v; want key B", id, kid, err)
		}
		ch, err := onlyB.GetChannel(ctx, id)
		if err != nil || ch.Config["routing_key"] != want {
			t.Errorf("channel %d = %v, %v; want %s", id, ch, err, want)
		}
	}
	again, err := storeB.ResealChannelSecrets(ctx)
	if err != nil || again != (SecretSealReport{}) {
		t.Fatalf("second reseal = %+v, %v; want nothing to do", again, err)
	}
}

func TestChannelSecrets_ResealCountsUnreadableAndLeavesIt(t *testing.T) {
	pool, ctx := requireDB(t)
	truncateChannels(t, pool)
	other := crypto.DeriveKey("rotation-key-c", []byte("rotation-salt-16"))
	storeC := NewNotificationStore(pool, nil).WithSecretKey(other)
	id := insertRawChannel(t, pool, uniqueName("s08-unreadable"), `{"routing_key":"rk-c"}`)
	if _, err := storeC.GetChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	before := rawRoutingKey(t, pool, id)
	storeB := NewNotificationStore(pool, nil).WithSecretKey(rotationKeyB)
	report, err := storeB.ResealChannelSecrets(ctx)
	if err != nil || report.Unreadable != 1 || report.Sealed+report.Rotated != 0 {
		t.Fatalf("report = %+v, %v; want 1 unreadable", report, err)
	}
	if after := rawRoutingKey(t, pool, id); after != before {
		t.Fatal("an unreadable channel secret was rewritten")
	}
}

func TestChannelSecrets_ResealWithoutKeyCountsPlaintext(t *testing.T) {
	pool, ctx := requireDB(t)
	truncateChannels(t, pool)
	id := insertRawChannel(t, pool, uniqueName("s08-nokey"), `{"routing_key":"rk-p"}`)
	report, err := NewNotificationStore(pool, nil).ResealChannelSecrets(ctx)
	if err != nil || report.Plaintext != 1 {
		t.Fatalf("report = %+v, %v; want 1 plaintext", report, err)
	}
	if raw := rawRoutingKey(t, pool, id); raw != "rk-p" || strings.HasPrefix(raw, "sage-enc") {
		t.Fatalf("routing_key = %q, want plaintext left as is", raw)
	}
}

func truncateChannels(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE sage.notification_channels CASCADE`); err != nil {
		t.Fatalf("truncate channels: %v", err)
	}
}
