package pgaudit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

const cicSQL = "CREATE INDEX CONCURRENTLY orders_status ON orders (status)"

const cicRecord = `AUDIT: SESSION,1,1,DDL,CREATE INDEX,INDEX,public.orders_status,"` +
	cicSQL + `",<not logged>`

// seedDecision records one standing-gate decision, with its own evidence
// id (evidence ids are unique), and returns its id.
func seedDecision(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.decision (feature,
		intent, verdict, risk_tier, reason, evidence_id, policy_version) VALUES
		('executor', 'create_index', 'execute', 'safe', 'missing index',
		 'ev-' || gen_random_uuid(), 3)
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	return id
}

func seedAction(t *testing.T, pool *pgxpool.Pool, decision int64, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, executed_at, outcome, decision_id) VALUES
		('create_index', $1, $2, 'success', $3) RETURNING id`, cicSQL, at,
		decision).Scan(&id); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	return id
}

// State transition (CHECK-05): a long action's tagged record is ingested
// before its action row exists. It is stored at once, by its tag, and the
// action's evidence pack holds it once the action is recorded 5 s later.
func TestIngestStoresTaggedRecordBeforeItsAction(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_tag_early")
	ctx := context.Background()
	decision := seedDecision(t, pool)
	own := sessionUser(t, pool)
	rec := entry("pg_sage action:"+itoa(decision), own, cicRecord)
	stats, err := NewCorrelator(pool, "orders", nil).Ingest(ctx, []logwatch.LogEntry{rec})
	if err != nil || stats.Stored != 1 {
		t.Fatalf("ingest before the action exists = %+v, %v; want stored", stats, err)
	}
	if count(t, pool, "correlated_by = 'action_tag' AND action_id IS NULL AND "+
		"application = 'pg_sage action:"+itoa(decision)+"' AND db_user = '"+own+"'") != 1 {
		t.Fatalf("tagged record not stored by its tag")
	}
	action := seedAction(t, pool, decision, rec.Timestamp.Add(5*time.Second))
	other := seedAction(t, pool, seedDecision(t, pool), rec.Timestamp.Add(6*time.Second))
	if !strings.Contains(packFile(t, pool, action, "pgaudit.json"), "orders_status") {
		t.Fatalf("action %d's pgaudit.json lacks its tagged record", action)
	}
	if strings.Contains(packFile(t, pool, other, "pgaudit.json"), "orders_status") {
		t.Fatalf("action %d of another decision holds the tagged record", other)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.PGAuditEvents, auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != 1 {
		t.Fatalf("tagged record not chained: %+v %v", rep, err)
	}
}

// Happy path: a tagged record whose action row already exists names it.
func TestIngestTaggedRecordLinksExistingAction(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_tag_late")
	ctx := context.Background()
	decision := seedDecision(t, pool)
	rec := entry("pg_sage action:"+itoa(decision), sessionUser(t, pool), cicRecord)
	action := seedAction(t, pool, decision, rec.Timestamp.Add(-time.Hour))
	if _, err := NewCorrelator(pool, "orders", nil).Ingest(ctx,
		[]logwatch.LogEntry{rec}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if count(t, pool, "correlated_by = 'action_tag' AND action_id = "+itoa(action)) != 1 {
		t.Fatalf("tagged record not linked to the existing action %d", action)
	}
}

// packFile builds action's evidence pack and returns one of its files.
func packFile(t *testing.T, pool *pgxpool.Pool, action int64, name string) string {
	t.Helper()
	pack, err := evidence.Build(context.Background(), evidence.Sources{DB: pool},
		evidence.Scope{Database: "orders", ActionID: action}, nil, time.Now())
	if err != nil {
		t.Fatalf("build pack of action %d: %v", action, err)
	}
	for _, f := range pack.Files {
		if f.Name == name {
			return string(f.Data)
		}
	}
	t.Fatalf("pack of action %d has no %s", action, name)
	return ""
}
