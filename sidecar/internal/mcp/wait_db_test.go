package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// wait_for_decision and get_request_status on real PostgreSQL, through
// the Streamable HTTP transport (mcp-01b). Each item is a real row in the
// table its own page reads; a person's decision is the store's own write.

// dbStatusBackend serves the status tools from the production reader.
type dbStatusBackend struct {
	recordingBackend
	*RequestStatusReader
}

func statusDB(t *testing.T) (*pgxpool.Pool, *fakeHub, func(Principal) string) {
	t.Helper()
	_, pool := newPostgresIntentAccess(t)
	reader := NewRequestStatusReader(func(_ context.Context, database string) (
		*pgxpool.Pool, error) {
		if database != "orders" {
			return nil, fmt.Errorf("database %q: %w", database, ErrRequestNotFound)
		}
		return pool, nil
	})
	hub := newFakeHub()
	server := NewServer(&dbStatusBackend{RequestStatusReader: reader}).
		WithDirectory(fleetOf("orders")).WithDecisionEvents(hub)
	urls := map[string]string{}
	urlFor := func(p Principal) string {
		if url, ok := urls[p.PrincipalID]; ok {
			return url
		}
		principal := p
		urls[p.PrincipalID] = httpServer(t, server, &principal).URL
		return urls[p.PrincipalID]
	}
	return pool, hub, urlFor
}

// httpTool posts one tools/call and returns its structured content or its
// error detail.
func httpTool(t *testing.T, url, tool, args string) (map[string]any, map[string]any) {
	t.Helper()
	resp, body := post(t, url, toolCall(tool, args), nil)
	require.Equal(t, 200, resp.StatusCode)
	result := objectMap(t, body["result"])
	content := objectMap(t, result["structuredContent"])
	if result["isError"] == true {
		return nil, objectMap(t, content["error"])
	}
	return content, nil
}

func startHTTPWait(t *testing.T, url, args string) <-chan map[string]any {
	t.Helper()
	done := make(chan map[string]any, 1)
	go func() {
		resp, body := post(t, url, toolCall(toolWaitForDecision, args), nil)
		if resp.StatusCode != 200 {
			done <- map[string]any{"http_status": resp.StatusCode}
			return
		}
		result, _ := body["result"].(map[string]any)
		content, _ := result["structuredContent"].(map[string]any)
		done <- content
	}()
	return done
}

func receiveHTTP(t *testing.T, done <-chan map[string]any, limit time.Duration) map[string]any {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(limit):
		t.Fatalf("the HTTP wait did not return within %s", limit)
	}
	return nil
}

// queueRow queues a change for principal on an open finding.
func queueRow(t *testing.T, pool *pgxpool.Pool, principal string) int {
	t.Helper()
	ctx := context.Background()
	var findingID, id int
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('wait_db', 'warning', 'table', $1, 'wait', '{}', 'open') RETURNING id`,
		fmt.Sprintf("public.wait_%d", time.Now().UnixNano())).Scan(&findingID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		(finding_id, proposed_sql, action_risk, action_type, principal_id)
		VALUES ($1, 'ANALYZE public.wait_a', 'safe', 'analyze', $2) RETURNING id`,
		findingID, principal).Scan(&id))
	return id
}

func TestWaitForDecisionDBApprovedExecutedVerifiedOverHTTP(t *testing.T) {
	pool, hub, urlFor := statusDB(t)
	ctx := context.Background()
	agent := agentPrincipalOf("dbwait")
	id := queueRow(t, pool, agent.PrincipalID)
	args := fmt.Sprintf(`{"handle":"change_approval:%d","timeout_seconds":30}`, id)
	done := startHTTPWait(t, urlFor(agent), args)
	hub.awaitSubscribers(t, 1)
	_, err := store.NewActionStore(pool).Approve(ctx, id, 3)
	require.NoError(t, err)
	approved := time.Now()
	hub.publish()
	got := receiveHTTP(t, done, time.Second)
	require.True(t, time.Since(approved) < time.Second, "returned within 1 s")
	require.Equal(t, "approved", got["state"])
	require.Equal(t, "person", got["decided_by_kind"])
	for _, step := range []struct{ sql, state string }{
		{`UPDATE sage.action_queue SET status = 'executed',
			verification_status = 'pending' WHERE id = $1`, "executed"},
		{`UPDATE sage.action_queue SET verification_status = 'verified'
			WHERE id = $1`, "verified"}} {
		done = startHTTPWait(t, urlFor(agent), args)
		hub.awaitSubscribers(t, 1)
		_, err = pool.Exec(ctx, step.sql, id)
		require.NoError(t, err)
		hub.publish()
		got = receiveHTTP(t, done, time.Second)
		require.Equal(t, step.state, got["state"])
	}
	require.Nil(t, got["next"], "verified is final")
}

func TestWaitForDecisionDBRejectedAndOtherPrincipalOverHTTP(t *testing.T) {
	pool, hub, urlFor := statusDB(t)
	agent, other := agentPrincipalOf("dbrej"), agentPrincipalOf("dbother")
	id := queueRow(t, pool, agent.PrincipalID)
	handle := fmt.Sprintf(`{"handle":"change_approval:%d"}`, id)
	_, failed := httpTool(t, urlFor(other), toolGetRequestStatus, handle)
	require.Equal(t, "not_found", failed["reason"], "CHECK-02 on a real row")
	_, failed = httpTool(t, urlFor(other), toolWaitForDecision, handle)
	require.Equal(t, "not_found", failed["reason"])
	done := startHTTPWait(t, urlFor(agent), handle)
	hub.awaitSubscribers(t, 1)
	require.NoError(t, store.NewActionStore(pool).Reject(context.Background(), id, 3, "no"))
	hub.publish()
	got := receiveHTTP(t, done, time.Second)
	require.Equal(t, "rejected", got["state"])
	require.Equal(t, "person", got["decided_by_kind"])
	require.Nil(t, got["next"])
}

func TestRequestStatusDBEveryKindPendingThenDecided(t *testing.T) {
	pool, _, urlFor := statusDB(t)
	agent := agentPrincipalOf("dbkinds")
	url := urlFor(agent)
	for _, kind := range dbKinds(t, pool, agent) {
		content, failed := httpTool(t, url, toolGetRequestStatus, `{"handle":"`+kind.handle+`"}`)
		require.Nil(t, failed, "%s: %v", kind.handle, failed)
		require.Equal(t, kind.before, content["state"], kind.handle)
		_, err := pool.Exec(context.Background(), kind.decide)
		require.NoError(t, err, kind.handle)
		content, failed = httpTool(t, url, toolGetRequestStatus, `{"handle":"`+kind.handle+`"}`)
		require.Nil(t, failed, "%s: %v", kind.handle, failed)
		require.Equal(t, kind.after, content["state"], kind.handle)
		_, failed = httpTool(t, urlFor(agentPrincipalOf("dbstranger")), toolGetRequestStatus,
			`{"handle":"`+kind.handle+`"}`)
		require.Equal(t, "not_found", failed["reason"], kind.handle)
	}
	_, failed := httpTool(t, url, toolGetRequestStatus, `{"handle":"plan:999999999"}`)
	require.Equal(t, "not_found", failed["reason"])
}

type dbKind struct{ handle, before, decide, after string }

// dbKinds seeds one held item of every kind for agent.
func dbKinds(t *testing.T, pool *pgxpool.Pool, agent Principal) []dbKind {
	t.Helper()
	ctx, actor := context.Background(), "mcp:"+agent.Actor
	var plan, grant, proposal int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, origin_ref,
		status, plan_hash, created_by) VALUES ('wait', 'agent', $1, 'proposed', $2, $3)
		RETURNING id`, agent.PrincipalID, strings.Repeat("a", 64), actor).Scan(&plan))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.guard_grant_requests
		(database_id, principal_id, capability, objects, duration_minutes, reason,
		 expires_at) VALUES (gen_random_uuid(), $1, 'read', '[]', 30, 'wait',
		 now() + interval '1 hour') RETURNING id`, agent.PrincipalID).Scan(&grant))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.policy (database_id, version,
		doc, status, proposed_by, proposed_principal_id) VALUES ($1, 1, '{}', 'proposed', $2,
		$3) RETURNING id`, time.Now().UnixNano()%1000000000+1000000, actor,
		agent.PrincipalID).Scan(&proposal))
	evidence := fmt.Sprintf("ev-wait-%d", time.Now().UnixNano())
	_, err := pool.Exec(ctx, `INSERT INTO sage.decision (feature, intent, verdict,
		risk_tier, reason, evidence_id, principal_id) VALUES ('mcp', 'optimize_query',
		'parked', 'moderate', 'outside_policy', $1, $2)`, evidence, agent.PrincipalID)
	require.NoError(t, err)
	queued := queueRow(t, pool, agent.PrincipalID)
	queuedEvidence := evidence + "-q"
	_, err = pool.Exec(ctx, `INSERT INTO sage.decision (feature, intent, verdict,
		risk_tier, reason, evidence_id, principal_id, queue_id) VALUES ('mcp',
		'optimize_query', 'queue_approval', 'moderate', 'approval_required', $1, $2, $3)`,
		queuedEvidence, agent.PrincipalID, queued)
	require.NoError(t, err)
	return []dbKind{
		{fmt.Sprintf("plan:%d", plan), "pending",
			fmt.Sprintf(`UPDATE sage.plan SET status = 'approved' WHERE id = %d`, plan),
			"approved"},
		{fmt.Sprintf("agent_request:%d", grant), "pending", fmt.Sprintf(`UPDATE
			sage.guard_grant_requests SET status = 'denied', decided_by = 3,
			decided_at = now() WHERE id = %d`, grant), "rejected"},
		{fmt.Sprintf("policy_exception:%d", proposal), "pending", fmt.Sprintf(`UPDATE
			sage.policy SET status = 'rejected' WHERE id = %d`, proposal), "rejected"},
		{"change:" + evidence, "held", fmt.Sprintf(`UPDATE sage.decision SET
			resolved_at = now() WHERE evidence_id = '%s'`, evidence), "resolved"},
		{"change:" + queuedEvidence, "pending", fmt.Sprintf(`UPDATE sage.action_queue SET
			status = 'rejected', decided_by = 3 WHERE id = %d`, queued), "rejected"},
	}
}
