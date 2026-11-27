package alerting

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// TestResolvePendingQuery_UsesFindingChannelStatusIndex: the per-channel
// lookups of resolvePendingQuery are served by the (finding_id, channel,
// status) index that bootstrap creates (alerting-04), even when the
// planner may not fall back to a sequential scan of sage.alert_log.
func TestResolvePendingQuery_UsesFindingChannelStatusIndex(t *testing.T) {
	pool := alertTestPool(t)
	ctx := context.Background()
	var def string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'sage' AND indexname = 'idx_alert_log_finding_channel_status'`,
	).Scan(&def); err != nil {
		t.Fatalf("index idx_alert_log_finding_channel_status missing: %v", err)
	}
	if !strings.HasSuffix(def, "(finding_id, channel, status)") {
		t.Fatalf("index definition = %s, want (finding_id, channel, status)", def)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET enable_seqscan") }()
	plan, err := testdb.Explain(ctx, conn, "", resolvePendingQuery, maxResolveAttempts)
	if err != nil {
		t.Fatalf("explain resolvePendingQuery: %v", err)
	}
	if n := plan.SeqScans("alert_log"); n != 0 {
		t.Errorf("resolvePendingQuery scans alert_log sequentially %d times:\n%s", n, plan)
	}
	used := plan.Has(func(p testdb.PlanNode) bool {
		return p.Index == "idx_alert_log_finding_channel_status"
	})
	if !used {
		t.Errorf("resolvePendingQuery does not use the new index:\n%s", plan)
	}
}

// TestResolveTargets_GroupsChannelsByFinding: rows of (finding, channel)
// come back one target per finding, in query order, with its channels.
func TestResolveTargets_GroupsChannelsByFinding(t *testing.T) {
	a := AlertFinding{ID: 7, Severity: "critical"}
	b := AlertFinding{ID: 3, Severity: "warning"}
	got := groupResolveRows([]resolveRow{
		{finding: a, channel: "slack"},
		{finding: a, channel: "pagerduty"},
		{finding: b, channel: "slack"},
	})
	if len(got) != 2 || got[0].finding.ID != 7 || got[1].finding.ID != 3 {
		t.Fatalf("targets = %+v, want findings 7 then 3", got)
	}
	if !got[0].channels["slack"] || !got[0].channels["pagerduty"] || len(got[0].channels) != 2 {
		t.Errorf("finding 7 channels = %v, want slack and pagerduty", got[0].channels)
	}
	if !got[1].channels["slack"] || len(got[1].channels) != 1 {
		t.Errorf("finding 3 channels = %v, want slack", got[1].channels)
	}
	if empty := groupResolveRows(nil); len(empty) != 0 {
		t.Errorf("no rows gave %d targets, want 0", len(empty))
	}
}

// TestSendResolutions_QueryErrorLoggedAndNothingSent: when the pending
// resolve query fails, the cycle logs it at ERROR with context and sends
// no resolve (error propagation; no database needed: the pool is lazy and
// its address refuses connections).
func TestSendResolutions_QueryErrorLoggedAndNothingSent(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatalf("build lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	slack := &resolveChannel{name: "slack"}
	logs := &logRecorder{}
	m := New(pool, ManagerConfig{}, map[string][]Channel{"critical": {slack}}, logs.log)

	m.sendResolutions(context.Background())
	if !logs.has("ERROR", "query pending resolve notifications: execute resolve pending query") {
		t.Errorf("query failure not logged at ERROR with context: %v", logs.lines)
	}
	slack.mu.Lock()
	sent := len(slack.resolves)
	slack.mu.Unlock()
	if sent != 0 {
		t.Errorf("resolves sent after a failed query: %d findings", sent)
	}
}
