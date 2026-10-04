package ask

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Fixtures for Ask Sage: one disposable database per package process with
// the sage schema bootstrapped once; every test starts from empty Ask,
// finding, action, incident and fact tables. Nothing here reaches a live
// LLM provider: models are fake OpenAI-compatible servers (fake_llm_test.go).

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/ask"))
}

var (
	bootstrapOnce sync.Once
	bootstrapErr  error
)

type fixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	bootstrapOnce.Do(func() { bootstrapErr = schema.Bootstrap(ctx, pool) })
	if bootstrapErr != nil {
		t.Fatalf("bootstrap sage schema: %v", bootstrapErr)
	}
	f := &fixture{t: t, ctx: ctx, pool: pool}
	f.exec(`TRUNCATE sage.ask_conversations, sage.ask_budget_day, sage.action_outcome,
		sage.action_queue, sage.facts, sage.incidents, sage.findings, sage.action_log
		CASCADE`)
	return f
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) count(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// findingSeed is one sage.findings row.
type findingSeed struct {
	Category, Severity, Object, Title, Recommendation, SQL, Rollback, Status string
	Detail                                                                   map[string]any
}

func (f *fixture) finding(s findingSeed) int64 {
	f.t.Helper()
	if s.Category == "" {
		s.Category = "missing_index"
	}
	if s.Severity == "" {
		s.Severity = "warning"
	}
	if s.Status == "" {
		s.Status = "open"
	}
	if s.Detail == nil {
		s.Detail = map[string]any{}
	}
	detail, err := json.Marshal(s.Detail)
	if err != nil {
		f.t.Fatal(err)
	}
	var id int64
	err = f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		rollback_sql, status, resolved_at) VALUES ($1, $2, 'table', $3, $4, $5,
		NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''), $9,
		CASE WHEN $9 = 'resolved' THEN now() END) RETURNING id`,
		s.Category, s.Severity, s.Object, s.Title, detail, s.Recommendation, s.SQL,
		s.Rollback, s.Status).Scan(&id)
	if err != nil {
		f.t.Fatalf("seed finding: %v", err)
	}
	return id
}

// action seeds an executed action with its verification outcome.
func (f *fixture) action(findingID int64, sql, verdict, tolerance string,
	predicted, observed map[string]any) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, rollback_sql, outcome, approved_by) VALUES ('create_index', $1, $2,
		'DROP INDEX CONCURRENTLY public.idx_orders_customer', 'success', 'alice')
		RETURNING id`, findingID, sql).Scan(&id); err != nil {
		f.t.Fatalf("seed action: %v", err)
	}
	if verdict == "" {
		return id
	}
	p, _ := json.Marshal(predicted)
	o, _ := json.Marshal(observed)
	f.exec(`INSERT INTO sage.action_outcome (action_log_id, action_class, predicted,
		prediction_method, verdict, tolerance, observed, reason, decided_at)
		VALUES ($1, 'index_create', $2, 'hypopg', $3, $4, $5, 'call-weighted mean', now())`,
		id, p, verdict, tolerance, o)
	return id
}

func (f *fixture) queued(findingID int64, sql, status string) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_queue (finding_id,
		proposed_sql, rollback_sql, action_risk, status, policy_decision)
		VALUES ($1, $2, 'DROP INDEX CONCURRENTLY public.idx_x', 'safe', $3,
		'queue_approval') RETURNING id`, findingID, sql, status).Scan(&id); err != nil {
		f.t.Fatalf("seed queue item: %v", err)
	}
	return id
}

func (f *fixture) incident(severity, rootCause string, resolved bool) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.incidents (severity, root_cause,
		source, database_name, affected_objects, resolved_at)
		VALUES ($1, $2, 'deterministic', 'testdb', ARRAY['public.orders'],
		CASE WHEN $3 THEN now() END) RETURNING id::text`, severity, rootCause,
		resolved).Scan(&id); err != nil {
		f.t.Fatalf("seed incident: %v", err)
	}
	return id
}

// testAskConfig is a generous budget so tests that are not about the
// budget never hit it.
func testAskConfig() config.AskConfig {
	return config.AskConfig{Enabled: true, DailyTokensPerDatabase: 5_000_000,
		DailyTokensPerUser: 2_000_000, MaxTokensPerQuestion: 40_000, RetentionDays: 30}
}

// deps is a service over the fixture with the given model; tests adjust
// the result before New.
func (f *fixture) deps(model *fakeLLM) Deps {
	d := Deps{Database: "testdb", Pool: f.pool, Config: testAskConfig(),
		Settings: config.DefaultConfig()}
	if model != nil {
		d.Model = model.client()
	}
	return d
}

func (f *fixture) service(d Deps) *Service {
	f.t.Helper()
	s, err := New(d)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return s
}

var (
	viewer   = Caller{Actor: "user:7"}
	operator = Caller{Actor: "user:1", MayPropose: true}
	agent    = Caller{Actor: "mcp:token:abc", MayPropose: true, Agent: true}
)
