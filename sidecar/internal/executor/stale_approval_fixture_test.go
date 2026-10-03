package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/verify"
)

// staleFixture is one optimizer CREATE INDEX recommendation on its own
// table, driven through RunCycle by a real standing gate, a real action
// queue and the durable recommendation store (dogfood lifeos, stale
// approval: queue items 2 and 3).
type staleFixture struct {
	pool      *pgxpool.Pool
	ctx       context.Context
	table     string
	database  string
	f         analyzer.Finding
	findingID int64
	rec       recommendation.Recommendation
	recs      *recommendation.Store
	queue     *store.ActionStore
	exec      *Executor
	trust     string
}

// queueRow is the part of a sage.action_queue row these tests assert.
type queueRow struct {
	ID          int
	Status      string
	Reason      string
	ProposedSQL string
}

func newStaleFixture(t *testing.T, trust string) *staleFixture {
	t.Helper()
	return newStaleFixtureWith(t, trust, unverifiedDetail())
}

// newStaleFixtureWith proposes the recommendation with detail as both the
// finding's detail and the revision's evidence.
func newStaleFixtureWith(t *testing.T, trust string, detail map[string]any) *staleFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	table := fmt.Sprintf("stale_appr_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+table+
		" (id bigint, status text)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	fx := &staleFixture{pool: pool, ctx: ctx, table: table, database: "stale_" + table,
		recs: recommendation.NewStore(pool), queue: store.NewActionStore(pool), trust: trust}
	fx.f = fx.indexFinding("idx_"+table+"_status", detail)
	fx.findingID = fx.insertFinding(t, fx.f)
	t.Cleanup(func() { fx.cleanup() })
	fx.exec = fx.newExecutor(t)
	res, err := fx.recs.Propose(ctx, analyzer.RecommendationProposal(fx.database, fx.f))
	if err != nil || res.Outcome != recommendation.OutcomeCreated {
		t.Fatalf("propose recommendation: %+v, %v", res, err)
	}
	fx.rec = res.Recommendation
	return fx
}

func unverifiedDetail() map[string]any {
	return map[string]any{"queryids": []int64{7}, "what_if_verdict": "unverified",
		"hypopg_validated": false, "what_if_reason": "HypoPG was not installed"}
}

func verifiedDetail() map[string]any {
	return map[string]any{"queryids": []int64{7}, "what_if_verdict": "verified",
		"hypopg_validated": true, "estimated_improvement_pct": 92.0}
}

func (fx *staleFixture) indexFinding(index string, detail map[string]any) analyzer.Finding {
	return analyzer.Finding{
		Category: "missing_index", Severity: "warning", ObjectType: "index",
		ObjectIdentifier: "public." + fx.table + "|btree(status)",
		Title:            "missing index on " + fx.table, Recommendation: "create it",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY " + index + " ON public." + fx.table +
			" USING btree (status);",
		RollbackSQL: `DROP INDEX CONCURRENTLY IF EXISTS "public"."` + index + `"`,
		ActionRisk:  "moderate", Detail: detail,
	}
}

func (fx *staleFixture) insertFinding(t *testing.T, f analyzer.Finding) int64 {
	t.Helper()
	detail, err := json.Marshal(f.Detail)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		rollback_sql) VALUES ($1, 'warning', 'index', $2, $3, $4, 'create it', $5, $6)
		RETURNING id`, f.Category, f.ObjectIdentifier, f.Title, detail, f.RecommendedSQL,
		f.RollbackSQL).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}

// newExecutor builds a production-shaped executor: real standing gate,
// real action queue, fake index verification.
func (fx *staleFixture) newExecutor(t *testing.T) *Executor {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.Level = fx.trust
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, true
	cfg.Trust.MaintenanceWindow = "always"
	var mu sync.Mutex
	var lines []string
	logFn := func(component, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, component+": "+fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() {
		if t.Failed() {
			mu.Lock()
			defer mu.Unlock()
			t.Logf("executor log:\n%s", strings.Join(lines, "\n"))
		}
	})
	exec := New(fx.pool, cfg, time.Now().Add(-90*24*time.Hour), logFn)
	exec.WithDatabaseName(fx.database)
	exec.WithActionStore(fx.queue, "auto")
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.indexVerification = newVerifiedIndexLifecycle(
		&fakeIndexVerifier{admission: verify.Admission{OK: true}},
		&fakeVerifiedIndexActions{})
	// The unattended profile with windows always open; its 24-hour limits
	// are raised because the package's other tests also spend them.
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.BlastRadius.MaxTablesPerWindow = 1 << 30
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 1 << 30
	exec.EnableStandingPolicyDocument(doc, nil)
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.Shutdown(sctx)
	})
	return exec
}

func (fx *staleFixture) cleanup() {
	ctx := context.Background()
	_, _ = fx.pool.Exec(ctx, `DELETE FROM sage.action_queue WHERE finding_id IN
		(SELECT id FROM sage.findings WHERE object_identifier = $1)`, fx.f.ObjectIdentifier)
	_, _ = fx.pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now() WHERE object_identifier = $1`, fx.f.ObjectIdentifier)
	_, _ = fx.pool.Exec(ctx, "DROP TABLE IF EXISTS public."+fx.table)
}

// setFindingDetail rewrites the open finding as the analyzer's upsert does
// each cycle (the optimizer re-verified it, or moved on to other SQL).
func (fx *staleFixture) setFinding(t *testing.T, sql string, detail map[string]any) {
	t.Helper()
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE sage.findings SET detail = $2,
		recommended_sql = $3, last_seen = now() WHERE id = $1`,
		fx.findingID, raw, sql); err != nil {
		t.Fatalf("update finding: %v", err)
	}
}

func (fx *staleFixture) markVerified(t *testing.T) {
	t.Helper()
	fx.setFinding(t, fx.f.RecommendedSQL, verifiedDetail())
}

func (fx *staleFixture) queueRows(t *testing.T) []queueRow {
	t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT id, status, COALESCE(reason, ''),
		proposed_sql FROM sage.action_queue WHERE finding_id = $1 ORDER BY id`, fx.findingID)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	defer rows.Close()
	var out []queueRow
	for rows.Next() {
		var q queueRow
		if err := rows.Scan(&q.ID, &q.Status, &q.Reason, &q.ProposedSQL); err != nil {
			t.Fatal(err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// onlyQueueRow returns the one queue row of the finding.
func (fx *staleFixture) onlyQueueRow(t *testing.T) queueRow {
	t.Helper()
	rows := fx.queueRows(t)
	if len(rows) != 1 {
		t.Fatalf("queue rows = %+v, want exactly one", rows)
	}
	return rows[0]
}

// queuePending runs the first cycle: the unverified index is queued.
func (fx *staleFixture) queuePending(t *testing.T) queueRow {
	t.Helper()
	fx.exec.RunCycle(fx.ctx, false)
	q := fx.onlyQueueRow(t)
	if q.Status != "pending" || fx.actions(t, fx.f.RecommendedSQL) != 0 {
		t.Fatalf("first cycle: queue=%+v actions=%d, want one pending proposal, no action",
			q, fx.actions(t, fx.f.RecommendedSQL))
	}
	return q
}

func (fx *staleFixture) setQueue(t *testing.T, id int, assignments string, args ...any) {
	t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE sage.action_queue SET `+assignments+
		` WHERE id = $1`, append([]any{id}, args...)...); err != nil {
		t.Fatalf("update queue row %d: %v", id, err)
	}
}

func (fx *staleFixture) actions(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (fx *staleFixture) indexExists(t *testing.T, index string) bool {
	t.Helper()
	var exists bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT to_regclass($1) IS NOT NULL`,
		"public."+index).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func (fx *staleFixture) index() string { return "idx_" + fx.table + "_status" }

// decisionVerdicts counts the standing gate's ledger rows on the target.
func (fx *staleFixture) decisionVerdicts(t *testing.T) map[string]int {
	t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT verdict || '/' || reason, count(*)
		FROM sage.decision WHERE target_objects = $1::jsonb GROUP BY 1`,
		`["`+fx.f.ObjectIdentifier+`"]`)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			t.Fatal(err)
		}
		out[key] = n
	}
	return out
}

func (fx *staleFixture) applyingTransitions(t *testing.T) int {
	t.Helper()
	history, err := fx.recs.Transitions(fx.ctx, fx.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, tr := range history {
		if tr.To == recommendation.StateApplying {
			n++
		}
	}
	return n
}
