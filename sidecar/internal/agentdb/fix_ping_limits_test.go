package agentdb

import (
	"errors"
	"fmt"
	"testing"
)

// G8-B24: random-token spraying is rate limited per deployment, and old
// failure rows are pruned instead of accumulating forever.
func TestPingTokenFailuresLimitedPerDeploymentAndPruned(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_fix_ping_spray"
	seedExpiredLiveDeployment(t, st, ctx, pool, id)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.agent_db_ping_token_failures
		(deployment_id, token_hash, reason, created_at)
		VALUES ($1, 'old', 'not_found', now()-interval '3 days')`, id); err != nil {
		t.Fatal(err)
	}
	var limited bool
	for i := 0; i < pingDeploymentFailureLimit+5; i++ {
		_, err := st.ValidatePingToken(ctx, id, fmt.Sprintf("random-%d", i))
		if errors.Is(err, ErrRateLimited) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("distinct random tokens were never rate limited")
	}
	var old int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.agent_db_ping_token_failures
		WHERE deployment_id=$1 AND token_hash='old'`, id).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old != 0 {
		t.Fatal("stale ping failure rows were not pruned")
	}
}
