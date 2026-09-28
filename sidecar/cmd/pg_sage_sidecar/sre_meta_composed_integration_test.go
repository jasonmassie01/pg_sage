package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M0 leftover, closed in M1: the collector -> incident -> notify
// composition in meta-db mode, through the real meta-db runtime
// constructor (applyMetaDatabaseCreate -> buildStoreDatabaseRuntime).
// Notification rules live in the meta database; the monitored database
// is a separate database holding a real blocking chain. The lock-chain
// fast path must open the incident without waiting for a second
// analyzer cycle (the analyzer interval here is an hour), and the
// notification carries the catalog-probe hypothesis.

func routeMetaIncidentEvents(
	t *testing.T, state *metaDBState, userID int, sink *hookSink,
) {
	t.Helper()
	router := wireRouter(WireParams{
		Cfg: cfg, Pool: state.Pool, FleetMgr: fleetMgr, MetaState: state,
	}).Handler
	created := adminAPIRequest(t, router, state.Pool, userID, http.MethodPost,
		"/api/v1/notifications/channels", map[string]any{
			"name": "sre-m1-meta-" + fmt.Sprint(time.Now().UnixNano()), "type": "slack",
			"config": map[string]string{"webhook_url": sink.srv.URL},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel = %d: %s", created.Code, created.Body.String())
	}
	var channel struct{ ID int }
	if err := json.NewDecoder(created.Body).Decode(&channel); err != nil {
		t.Fatal(err)
	}
	rule := adminAPIRequest(t, router, state.Pool, userID, http.MethodPost,
		"/api/v1/notifications/rules", map[string]any{
			"channel_id": channel.ID, "event": "incident_detected",
			"min_severity": "info",
		})
	if rule.Code != http.StatusCreated {
		t.Fatalf("create rule = %d: %s", rule.Code, rule.Body.String())
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			"DELETE FROM sage.notification_rules WHERE channel_id = $1",
			"DELETE FROM sage.notification_log WHERE channel_id = $1",
			"DELETE FROM sage.notification_channels WHERE id = $1"} {
			_, _ = state.Pool.Exec(context.Background(), sql, channel.ID)
		}
	})
}

func waitDeliveries(sink *hookSink, timeout time.Duration, needles ...string) []string {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); {
		if got := sink.matching(needles...); len(got) > 0 {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// registerMetaMonitored creates a monitored database next to the meta
// database and registers it through the meta-db runtime constructor.
func registerMetaMonitored(
	t *testing.T, ctx context.Context, state *metaDBState, input store.DatabaseInput,
	userID int,
) (*fleet.DatabaseInstance, string) {
	t.Helper()
	dsnB := createFleetDatabase(t, ctx, state.Pool, testdb.SkipUnlessLive(t))
	u, err := url.Parse(dsnB)
	if err != nil {
		t.Fatalf("parse monitored dsn: %v", err)
	}
	input.Name, input.DatabaseName = "meta_sre_db", strings.TrimPrefix(u.Path, "/")
	if _, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID); err != nil {
		t.Fatalf("register monitored database: %v", err)
	}
	inst := fleetMgr.GetInstance(input.Name)
	if inst == nil {
		t.Fatal("monitored database was not registered")
	}
	return inst, dsnB
}

func TestComposedSRE_MetaDBCollectorIncidentNotify(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	resetNotifyDispatchers(t)
	oldMeta := globalMetaState
	t.Cleanup(func() { globalMetaState = oldMeta })
	globalMetaState = state
	cfg.MetaDB = "postgres://fixture"
	cfg.NotificationPolicy.AllowPrivateTargets = true
	cfg.RCA.LockChainIntervalSeconds = 1
	cfg.Analyzer.LockChain.MinBlockedThreshold = 1
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	sink := newHookSink(t)
	routeMetaIncidentEvents(t, state, userID, sink)
	inst, dsnB := registerMetaMonitored(t, ctx, state, input, userID)
	chain := startComposedChain(t, ctx, dsnB, inst.Pool)

	det := waitDeliveries(sink, 30*time.Second, "Incident detected",
		"Database: meta_sre_db", "lock_contention")
	if len(det) != 1 {
		t.Fatalf("detected deliveries = %d within 30 s, want 1 from the fast "+
			"path; all: %v", len(det), sink.matching())
	}
	for _, want := range []string{"Summary (deterministic)",
		"Likely (H1, confidence", "idle-in-transaction holder",
		fmt.Sprintf("pid %d", chain.holderPID)} {
		if !strings.Contains(det[0], want) {
			t.Fatalf("delivery lacks %q: %s", want, det[0])
		}
	}
	var n int
	if err := inst.Pool.QueryRow(ctx, `SELECT count(*) FROM sage.incidents
		WHERE database_name = 'meta_sre_db' AND resolved_at IS NULL
		AND causal_chain::text LIKE $1`,
		fmt.Sprintf(`%%"pid": %d%%`, chain.holderPID)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("open incidents naming the holder = %d (%v), want 1", n, err)
	}
	chain.release()
	stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := fleet.ShutdownInstance(stopCtx, inst); err != nil {
		t.Fatalf("instance shutdown did not drain its workers: %v", err)
	}
}
