package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// D1: a stored policy that the stricter validation now rejects (an unknown
// refusal token written through the API) fails closed. Startup must say so
// in the log instead of leaving every action silently blocked.
func TestEnableStandingPolicyLogsInvalidStoredDocument(t *testing.T) {
	pool, ctx := requireDB(t)
	const databaseID = 990301
	raw, err := policy.MarshalDocument(policy.StaffedProfile())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	invalid := strings.Replace(string(raw), `"rls_change"`, `"drop_everything"`, 1)
	cleanup := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.policy WHERE database_id = $1`, databaseID)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.policy
		(database_id, version, doc, status, proposed_by, ratified_by, ratified_at,
		 activated_at, schema_version)
		VALUES ($1, 1, $2, 'active', 'test', 'test', now(), now(), 3)`,
		databaseID, invalid); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	var mu sync.Mutex
	var logs []string
	logFn := func(_ string, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	exec := New(pool, config.DefaultConfig(), time.Time{}, logFn)
	id := databaseID

	if err := exec.EnableStandingPolicy(ctx, "staffed", &id); err != nil {
		t.Fatalf("EnableStandingPolicy: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "drop_everything") || !strings.Contains(joined, "blocked") {
		t.Fatalf("logs %q must name the invalid token and say actions are blocked", joined)
	}
}
