package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// cardFixture is one database ("orders") with a real executor behind the
// unattended standing policy and one queued ANALYZE on its own table: the
// smallest action that really runs through Executor.Apply.
type cardFixture struct {
	pool      *pgxpool.Pool
	mgr       *fleet.DatabaseManager
	exec      *executor.Executor
	table     string
	sql       string
	findingID int
	queueID   int
}

func newCardFixture(t *testing.T) *cardFixture {
	t.Helper()
	pool := surfacePool(t)
	ctx := context.Background()
	fx := &cardFixture{pool: pool,
		table: fmt.Sprintf("card_api_%d", time.Now().UnixNano())}
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+fx.table+
		" (id int); INSERT INTO public."+fx.table+" SELECT generate_series(1, 50)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The approvals really ran through Executor.Apply, which leaves change
		// leases that reference their decisions; other tests of this package
		// delete every decision, so the leases go with the fixture.
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.change_lease
			WHERE intent LIKE '%' || $1 || '%' OR object_key LIKE '%' || $1 || '%'
			   OR COALESCE(object_name, '') LIKE '%' || $1 || '%'`, fx.table)
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+fx.table)
	})
	fx.sql = "ANALYZE public." + fx.table
	fx.findingID, fx.queueID = fx.queue(t, fx.sql)
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	fx.exec = executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.BlastRadius.MaxTablesPerWindow = 1 << 30
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 1 << 30
	fx.exec.EnableStandingPolicyDocument(doc, nil)
	fx.exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	fx.mgr = fleet.NewManager(cfg)
	fx.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Executor: fx.exec, Status: &fleet.InstanceStatus{}})
	return fx
}

// queue inserts an open stale-statistics finding for sql and its pending
// approval item.
func (fx *cardFixture) queue(t *testing.T, sql string) (int, int) {
	t.Helper()
	ctx := context.Background()
	var findingID int
	if err := fx.pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		status) VALUES ('stale_statistics', 'warning', 'table', $1,
		'Stale statistics on ' || $1, '{"n_mod_since_analyze": 5000,
		"last_analyze": "2026-09-01T00:00:00Z"}', 'Run ANALYZE', $2, 'open')
		RETURNING id`, "public."+fx.table, sql).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	id, err := store.NewActionStore(fx.pool).ProposeWithMetadata(ctx, nil, findingID, sql,
		"", "safe", store.ActionProposalMetadata{ActionType: "analyze_table",
			PolicyDecision: "queue_approval"})
	if err != nil {
		t.Fatal(err)
	}
	return findingID, id
}

func (fx *cardFixture) router(t *testing.T, user *auth.User) http.Handler {
	t.Helper()
	return actionRouter(t, fx.mgr, user)
}

func (fx *cardFixture) status(t *testing.T) (string, string) {
	t.Helper()
	var status, reason string
	if err := fx.pool.QueryRow(context.Background(), `SELECT status, COALESCE(reason, '')
		FROM sage.action_queue WHERE id = $1`, fx.queueID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

// executions counts the action_log rows of the fixture's SQL.
func (fx *cardFixture) executions(t *testing.T) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, fx.sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (fx *cardFixture) cardHash(t *testing.T) string {
	t.Helper()
	code, body := cardCall(t, fx.router(t, testAdminUser()), http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", fx.queueID), nil)
	if code != http.StatusOK {
		t.Fatalf("get card: %d %v", code, body)
	}
	card, _ := body["card"].(map[string]any)
	hash, _ := card["card_hash"].(string)
	if hash == "" {
		t.Fatalf("card has no hash: %v", body)
	}
	return hash
}

func cardCall(t *testing.T, h http.Handler, method, path string,
	body map[string]any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	return serveJSON(h, req)
}

func operatorUser() *auth.User { return &auth.User{ID: 2, Email: "op@x", Role: "operator"} }

func viewerUser() *auth.User { return &auth.User{ID: 3, Email: "view@x", Role: "viewer"} }
