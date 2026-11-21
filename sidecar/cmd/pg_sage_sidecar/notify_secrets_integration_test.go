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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
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

	router := testWireRouter(WireParams{
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
	res, err := dispatcher.Submit(context.Background(), notify.Event{
		Type: "action_failed", Severity: "warning", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// alerting-06: with a process lifetime (prepareMetaGlobals sets
	// shutdownCtx) the runtime dispatcher queues action events in the
	// outbox, and its drain decrypts the sealed channel and delivers.
	if res != (notify.DispatchResult{Enqueued: 1}) {
		t.Fatalf("submit = %+v, want one queued delivery", res)
	}
	if _, err := dispatcher.DrainOutbox(context.Background()); err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
	assertOutboxDelivered(t, state.Pool, channel.ID, hits)
}

// assertOutboxDelivered waits for channelID's one outbox entry to be sent
// (by DrainOutbox or the background drain, which lease it, never both)
// and checks the webhook got exactly that one delivery.
func assertOutboxDelivered(t *testing.T, pool *pgxpool.Pool, channelID int,
	hits *atomic.Int32) {
	t.Helper()
	var status string
	for deadline := time.Now().Add(20 * time.Second); ; {
		err := pool.QueryRow(context.Background(), `SELECT string_agg(status, ',')
			FROM sage.notification_outbox WHERE channel_id = $1`, channelID).Scan(&status)
		if err != nil {
			t.Fatalf("read outbox: %v", err)
		}
		if status == "sent" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := hits.Load(); status != "sent" || got != 1 {
		t.Fatalf("outbox status %q, webhook deliveries = %d, want sent once "+
			"(decrypt + deliver)", status, got)
	}
}

// G7-B21: without allow_private_targets the API refuses an internal target.
func TestNotificationPrivateTargetRefusedByDefault(t *testing.T) {
	state, _, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	resetNotifyDispatchers(t)
	cfg.MetaDB = "postgres://fixture"
	sink, _ := webhookSink(t)
	router := testWireRouter(WireParams{
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
	cfg.Mode = config.ModeMeta
	if key := notificationSecretKey(p); key != nil {
		t.Fatal("meta mode derived a key from a non-meta pool (would write sidecar rows)")
	}
}

// alerting-03: an approval_needed rule created through the API without a
// min_severity gets the default floor (warning), and a fact card still
// reaches it and is recorded as one sent notification_log row.
func TestFactCardDeliveredUnderDefaultApprovalRule(t *testing.T) {
	state, _, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	resetNotifyDispatchers(t)
	oldMeta := globalMetaState
	t.Cleanup(func() { globalMetaState = oldMeta })
	globalMetaState = state
	cfg.MetaDB = "postgres://fixture"
	cfg.NotificationPolicy.AllowPrivateTargets = true
	sink, hits := webhookSink(t)
	ctx := context.Background()

	router := testWireRouter(WireParams{
		Cfg: cfg, Pool: state.Pool, FleetMgr: fleetMgr, MetaState: state,
	}).Handler
	channelID := createDefaultApprovalRule(t, router, state.Pool, userID, sink.URL)

	card := notify.FactProposedEvent(notify.FactCardRef{Database: "orders",
		FactID: 41, FactHash: "fh", Title: "idx_orders_run is app-owned"},
		"Evidence: recreated 8 times")
	dispatcher := sharedNotifyDispatcher(notificationControlPool(state, nil))
	if err := dispatcher.Dispatch(ctx, card); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("webhook deliveries = %d, want 1 fact card", got)
	}
	var sent int
	if err := state.Pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.notification_log
		  WHERE channel_id = $1 AND event = 'approval_needed'
		    AND status = 'sent' AND subject = $2`,
		channelID, card.Subject).Scan(&sent); err != nil {
		t.Fatalf("read notification_log: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent fact card log rows = %d, want 1", sent)
	}
}

// createDefaultApprovalRule creates a Slack channel to sinkURL and an
// approval_needed rule without min_severity through the API, checks the
// stored default, and returns the channel id.
func createDefaultApprovalRule(
	t *testing.T, router http.Handler, pool *pgxpool.Pool, userID int, sinkURL string,
) int {
	t.Helper()
	created := adminAPIRequest(t, router, pool, userID, http.MethodPost,
		"/api/v1/notifications/channels", map[string]any{
			"name": "facts-" + t.Name(), "type": "slack",
			"config": map[string]string{"webhook_url": sinkURL + "/hook"},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel = %d: %s", created.Code, created.Body.String())
	}
	var channel struct{ ID int }
	if err := json.NewDecoder(created.Body).Decode(&channel); err != nil {
		t.Fatal(err)
	}
	rule := adminAPIRequest(t, router, pool, userID, http.MethodPost,
		"/api/v1/notifications/rules", map[string]any{
			"channel_id": channel.ID, "event": "approval_needed",
		})
	if rule.Code != http.StatusCreated {
		t.Fatalf("create rule = %d: %s", rule.Code, rule.Body.String())
	}
	var minSeverity string
	if err := pool.QueryRow(context.Background(),
		`SELECT min_severity FROM sage.notification_rules
			WHERE channel_id = $1 AND event = 'approval_needed'`,
		channel.ID).Scan(&minSeverity); err != nil {
		t.Fatalf("read rule row: %v", err)
	}
	if minSeverity != "warning" {
		t.Fatalf("API default min_severity = %q, want warning", minSeverity)
	}
	return channel.ID
}
