package retention

import (
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

// Perf storage phase: the 18 lazily created agent_db_* tables and the
// action queue had no retention at all. Append-only rows age out; rows
// that back a live object, a ledger or an audit trail are kept.

func ensureAgentSchema(t *testing.T) {
	t.Helper()
	pool, ctx := requireDB(t)
	if err := agentdb.NewStore(pool).Ensure(ctx); err != nil {
		t.Fatalf("agentdb schema: %v", err)
	}
}

func agentDeployment(t *testing.T, tag string) string {
	t.Helper()
	_, ctx := requireDB(t)
	id := tag + "_dep"
	execRetry(t, ctx, `INSERT INTO sage.agent_db_deployments (deployment_id, tenant_id,
		agent_id) VALUES ($1, 't', 'a')`, id)
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM sage.agent_db_deployments
			WHERE deployment_id = $1`, id)
	})
	return id
}

func TestRunOnce_AgentPingsAgeOutButTheLatestStays(t *testing.T) {
	ensureAgentSchema(t)
	pool, ctx := requireDB(t)
	tag := uniqueTag("pings")
	stale := agentDeployment(t, tag+"_stale")
	live := agentDeployment(t, tag+"_live")
	execRetry(t, ctx, `INSERT INTO sage.agent_db_pings (deployment_id, status, created_at)
		VALUES ($1, 'ok', now() - interval '200 days'), ($1, 'ok', now() - interval '150 days'),
		       ($2, 'ok', now() - interval '200 days'), ($2, 'ok', now() - interval '1 hour')`,
		stale, live)
	New(pool, allDays(90), noopLog).RunOnce(ctx)
	// A deployment that stopped pinging keeps its last ping as evidence.
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.agent_db_pings
		WHERE deployment_id = $1 AND created_at < now() - interval '149 days'`, stale); n != 1 {
		t.Fatalf("stale deployment kept %d pings, want its newest only", n)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.agent_db_pings
		WHERE deployment_id = $1`, live); n != 1 {
		t.Fatalf("live deployment kept %d pings, want the recent one", n)
	}
}

func TestRunOnce_AgentTokensAndFailuresAgeOut(t *testing.T) {
	ensureAgentSchema(t)
	pool, ctx := requireDB(t)
	tag := uniqueTag("tokens")
	dep := agentDeployment(t, tag)
	execRetry(t, ctx, `INSERT INTO sage.agent_db_ping_tokens (token_id, deployment_id,
		agent_id, token_hash, expires_at, revoked_at) VALUES
		($1 || '_expired', $2, 'a', $1 || 'h1', now() - interval '400 days', NULL),
		($1 || '_revoked', $2, 'a', $1 || 'h2', now() + interval '1 day',
		 now() - interval '400 days'),
		($1 || '_active', $2, 'a', $1 || 'h3', now() + interval '1 day', NULL)`, tag, dep)
	execRetry(t, ctx, `INSERT INTO sage.agent_db_agent_tokens (token_id, tenant_id, agent_id,
		token_hash, expires_at) VALUES
		($1 || '_old', 't', 'a', $1 || 'a1', now() - interval '400 days'),
		($1 || '_new', 't', 'a', $1 || 'a2', now() + interval '30 days')`, tag)
	execRetry(t, ctx, `INSERT INTO sage.agent_db_ping_token_failures (deployment_id,
		token_hash, reason, created_at) VALUES ($1, 'x', 'old', now() - interval '400 days'),
		($1, 'x', 'new', now())`, dep)
	New(pool, allDays(365), noopLog).RunOnce(ctx)
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.agent_db_ping_tokens
		WHERE deployment_id = $1`, dep); n != 1 {
		t.Fatalf("%d ping tokens remain, want the active one", n)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.agent_db_agent_tokens
		WHERE token_id LIKE $1 || '%'`, tag); n != 1 {
		t.Fatalf("%d agent tokens remain, want the unexpired one", n)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.agent_db_ping_token_failures
		WHERE deployment_id = $1`, dep); n != 1 {
		t.Fatalf("%d token failures remain, want the recent one", n)
	}
}

func TestRunOnce_AgentProvisionAttemptsKeepTheLatestPerDeployment(t *testing.T) {
	ensureAgentSchema(t)
	pool, ctx := requireDB(t)
	dep := agentDeployment(t, uniqueTag("attempts"))
	execRetry(t, ctx, `INSERT INTO sage.agent_db_provision_attempts (deployment_id, kind,
		status, stdout, created_at) VALUES
		($1, 'create', 'failed', repeat('x', 1000), now() - interval '500 days'),
		($1, 'create', 'ok', 'done', now() - interval '450 days')`, dep)
	New(pool, allDays(365), noopLog).RunOnce(ctx)
	var status string
	queryRetry(t, ctx, `SELECT string_agg(status, ',') FROM sage.agent_db_provision_attempts
		WHERE deployment_id = '`+dep+`'`, &status)
	if status != "ok" {
		t.Fatalf("attempts left = %q, want only the latest (ok)", status)
	}
}

// An estimate an authorization was issued on is evidence of a live
// operation (deleting it would cascade to the authorization and receipt).
func TestRunOnce_AgentLiveEstimatesKeepAuthorizedOnes(t *testing.T) {
	ensureAgentSchema(t)
	pool, ctx := requireDB(t)
	tag := uniqueTag("estimates")
	dep := agentDeployment(t, tag)
	execRetry(t, ctx, `INSERT INTO sage.agent_db_live_plans (plan_hash, deployment_id, payload)
		VALUES ($1, $2, '{}')`, tag, dep)
	execRetry(t, ctx, `INSERT INTO sage.agent_db_live_estimates (estimate_id, plan_hash,
		deployment_id, payload, expires_at, created_at) VALUES
		($1 || '_used', $1, $2, '{}', now() - interval '400 days', now() - interval '401 days'),
		($1 || '_unused', $1, $2, '{}', now() - interval '400 days', now() - interval '401 days'),
		($1 || '_live', $1, $2, '{}', now() + interval '1 hour', now())`, tag, dep)
	execRetry(t, ctx, `INSERT INTO sage.agent_db_live_authorizations (authorization_id,
		estimate_id, plan_hash, deployment_id, operation, requester_id, idempotency_key,
		payload, expires_at) VALUES ($1 || '_auth', $1 || '_used', $1, $2, 'create', 'r',
		$1, '{}', now() - interval '399 days')`, tag, dep)
	New(pool, allDays(365), noopLog).RunOnce(ctx)
	rows, err := pool.Query(ctx, `SELECT estimate_id FROM sage.agent_db_live_estimates
		WHERE deployment_id = $1 ORDER BY 1`, dep)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, s)
	}
	if !slices.Equal(kept, []string{tag + "_live", tag + "_used"}) {
		t.Fatalf("estimates kept = %v", kept)
	}
}

func TestRunOnce_AgentMonitoringWorkOfDeletedDeploymentsAgesOut(t *testing.T) {
	ensureAgentSchema(t)
	pool, ctx := requireDB(t)
	tag := uniqueTag("work")
	execRetry(t, ctx, `INSERT INTO sage.agent_db_monitoring_work (work_id, deployment_id,
		tenant_id, provider, physical_target_key, tier, status, next_due_at, revoked_at)
		VALUES ($1 || '_revoked', 'd', 't', 'p', 'k1', 'light', 'revoked', now(),
		        now() - interval '400 days'),
		       ($1 || '_queued', 'd', 't', 'p', 'k2', 'light', 'queued',
		        now() - interval '400 days', NULL)`, tag)
	New(pool, allDays(365), noopLog).RunOnce(ctx)
	var left string
	queryRetry(t, ctx, `SELECT string_agg(status, ',') FROM sage.agent_db_monitoring_work
		WHERE work_id LIKE '`+tag+`%'`, &left)
	if left != "queued" {
		t.Fatalf("monitoring work left = %q, want the queued item only", left)
	}
}

// The approval queue keeps pending work, rows an action, a decision or an
// SRE proposal still points at; decided rows age out with actions_days.
func TestRunOnce_ActionQueueTerminalRowsAgeOut(t *testing.T) {
	pool, ctx := requireDB(t)
	tag := uniqueTag("queue")
	insert := func(status, extra string) int64 {
		return insertID(t, ctx, `INSERT INTO sage.action_queue (proposed_sql, action_risk,
			status, proposed_at, decided_at, expires_at, reason)
			VALUES ('SELECT 1', 'safe', $1, now() - interval '400 days',
			        now() - interval '400 days', now() - interval '393 days'
			        `+extra+`, $2) RETURNING id`, status, tag)
	}
	pending := insert("pending", "")
	rejected := insert("rejected", "")
	expired := insert("expired", "")
	failedRetrying := insertID(t, ctx, `INSERT INTO sage.action_queue (proposed_sql,
		action_risk, status, proposed_at, expires_at, reason) VALUES ('SELECT 1', 'safe',
		'failed', now() - interval '400 days', now() + interval '1 day', $1) RETURNING id`, tag)
	logID := insertID(t, ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome, executed_at) VALUES ('t', 'SELECT 1', 'success', now()) RETURNING id`)
	executedLive := insertID(t, ctx, `INSERT INTO sage.action_queue (proposed_sql, action_risk,
		status, proposed_at, decided_at, action_log_id, reason) VALUES ('SELECT 1', 'safe',
		'executed', now() - interval '400 days', now() - interval '400 days', $1, $2)
		RETURNING id`, logID, tag)
	byDecision := insert("rejected", "")
	execRetry(t, ctx, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier, reason,
		evidence_id, queue_id) VALUES ('t', 'i', 'queue_approval', 'safe', 'r', $1, $2)`,
		tag, byDecision)
	New(pool, allDays(365), noopLog).RunOnce(ctx)
	rows, err := pool.Query(ctx, `SELECT id FROM sage.action_queue WHERE reason = $1
		ORDER BY id`, tag)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, id)
	}
	want := []int64{pending, failedRetrying, executedLive, byDecision}
	slices.Sort(want)
	if !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v (rejected %d and expired %d purged)", kept, want,
			rejected, expired)
	}
	execRetry(t, ctx, `DELETE FROM sage.decision WHERE evidence_id = $1`, tag)
	execRetry(t, ctx, `DELETE FROM sage.action_queue WHERE reason = $1`, tag)
	execRetry(t, ctx, `DELETE FROM sage.action_log WHERE id = $1`, logID)
}
