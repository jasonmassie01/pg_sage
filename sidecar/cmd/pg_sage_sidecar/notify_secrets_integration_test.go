package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// webhookSink counts deliveries to a fake Slack webhook on loopback.
func webhookSink(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(server.Close)
	return server, &hits
}

func adminAPIRequest(
	t *testing.T, handler http.Handler, pool *pgxpool.Pool, userID int,
	method, path string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	session, err := auth.CreateSession(context.Background(), pool, userID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sage_session", Value: session})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func resetNotifyDispatchers(t *testing.T) {
	t.Helper()
	notifyDispatchersMu.Lock()
	previous := notifyDispatchers
	notifyDispatchers = map[*pgxpool.Pool]*notify.Dispatcher{}
	notifyDispatchersMu.Unlock()
	t.Cleanup(func() {
		notifyDispatchersMu.Lock()
		notifyDispatchers = previous
		notifyDispatchersMu.Unlock()
	})
}

// G7-B20 / G7-B21: a channel created through the API in meta mode is sealed
// at rest with the meta encryption key, and the runtime dispatcher (same
// key, same target policy) still decrypts and delivers it.
func TestMetaNotificationSecretEncryptedAtRestAndDelivered(t *testing.T) {
	state, _, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	resetNotifyDispatchers(t)
	oldMeta := globalMetaState
	t.Cleanup(func() { globalMetaState = oldMeta })
	globalMetaState = state
	cfg.MetaDB = "postgres://fixture"
	cfg.NotificationPolicy.AllowPrivateTargets = true
	sink, hits := webhookSink(t)

	router := wireRouter(WireParams{
		Cfg: cfg, Pool: state.Pool, FleetMgr: fleetMgr, MetaState: state,
	}).Handler
	created := adminAPIRequest(t, router, state.Pool, userID, http.MethodPost,
		"/api/v1/notifications/channels", map[string]any{
			"name": "sealed-" + t.Name(), "type": "slack",
			"config": map[string]string{"webhook_url": sink.URL + "/hook"},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel = %d: %s", created.Code, created.Body.String())
	}
	var channel struct{ ID int }
	if err := json.NewDecoder(created.Body).Decode(&channel); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := state.Pool.QueryRow(context.Background(),
		`SELECT config::text FROM sage.notification_channels WHERE id = $1`,
		channel.ID).Scan(&raw); err != nil {
		t.Fatalf("read channel row: %v", err)
	}
	if strings.Contains(raw, sink.URL) || strings.Contains(raw, "/hook") {
		t.Fatalf("webhook secret stored in plaintext: %s", raw)
	}
	rule := adminAPIRequest(t, router, state.Pool, userID, http.MethodPost,
		"/api/v1/notifications/rules", map[string]any{
			"channel_id": channel.ID, "event": "action_failed",
			"min_severity": "info",
		})
	if rule.Code != http.StatusCreated {
		t.Fatalf("create rule = %d: %s", rule.Code, rule.Body.String())
	}

	dispatcher := sharedNotifyDispatcher(notificationControlPool(state, nil))
	err := dispatcher.Dispatch(context.Background(), notify.Event{
		Type: "action_failed", Severity: "warning", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("webhook deliveries = %d, want 1 (decrypt + deliver)", got)
	}
}

// G7-B21: without allow_private_targets the API refuses an internal target.
func TestNotificationPrivateTargetRefusedByDefault(t *testing.T) {
	state, _, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	resetNotifyDispatchers(t)
	cfg.MetaDB = "postgres://fixture"
	sink, _ := webhookSink(t)
	router := wireRouter(WireParams{
		Cfg: cfg, Pool: state.Pool, FleetMgr: fleetMgr, MetaState: state,
	}).Handler
	created := adminAPIRequest(t, router, state.Pool, userID, http.MethodPost,
		"/api/v1/notifications/channels", map[string]any{
			"name": "private-" + t.Name(), "type": "slack",
			"config": map[string]string{"webhook_url": sink.URL},
		})
	if created.Code != http.StatusBadRequest {
		t.Fatalf("loopback webhook accepted by default: %d %s",
			created.Code, created.Body.String())
	}
}

// Standalone and YAML-fleet control pools derive the channel key from the
// same passphrase + persisted salt, so API writes and runtime reads agree.
func TestNotificationSecretKeyDerivation(t *testing.T) {
	prepareMetaGlobals(t)
	resetNotifyDispatchers(t)
	ctx := context.Background()
	p, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}

	cfg.Mode, cfg.EncryptionKey = "standalone", ""
	if key := notificationSecretKey(p); key != nil {
		t.Fatalf("key without passphrase = %d bytes, want nil", len(key))
	}
	resetNotifyDispatchers(t)
	cfg.EncryptionKey = "standalone-passphrase"
	key := notificationSecretKey(p)
	salt, err := schema.ReadOrCreateKDFSalt(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if want := crypto.DeriveKey("standalone-passphrase", salt); !bytes.Equal(key, want) {
		t.Fatalf("derived key mismatch: got %d bytes", len(key))
	}
	if again := notificationSecretKey(p); !bytes.Equal(again, key) {
		t.Fatal("key is not stable across calls")
	}
	resetNotifyDispatchers(t)
	cfg.Mode = "extension"
	if key := notificationSecretKey(p); key != nil {
		t.Fatal("extension mode derived a key (would write sidecar rows)")
	}
}
