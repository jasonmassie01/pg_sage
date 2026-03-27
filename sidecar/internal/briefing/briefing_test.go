package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
}

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	testPoolOnce.Do(func() {
		dsn := testDSN()
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		poolCfg.MaxConns = 1
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		// Ensure sage schema exists.
		if err := schema.Bootstrap(ctx, testPool); err != nil {
			testPoolErr = fmt.Errorf("bootstrap: %w", err)
			testPool.Close()
			testPool = nil
			return
		}
		schema.ReleaseAdvisoryLock(ctx, testPool)
	})
	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, ctx
}

func noopLog(_, _ string, _ ...any) {}

func newTestWorker(cfg *config.Config) *Worker {
	return &Worker{
		pool:  nil,
		cfg:   cfg,
		llm:   nil,
		logFn: noopLog,
	}
}

func TestBuildStructured_EmptyFindings(t *testing.T) {
	w := newTestWorker(&config.Config{})
	result := w.buildStructured("[]", `{"db_size":"100 MB","connections":5}`, "[]")

	if !strings.Contains(result, "# pg_sage Health Briefing") {
		t.Error("expected header in briefing output")
	}
	if !strings.Contains(result, "## System Overview") {
		t.Error("expected system overview section")
	}
	if !strings.Contains(result, "0 critical, 0 warning, 0 info") {
		t.Error("expected zero counts for empty findings")
	}
	// No recent actions section when actions list is empty.
	if strings.Contains(result, "## Recent Actions") {
		t.Error("should not show Recent Actions section for empty list")
	}
}

func TestBuildStructured_WithFindings(t *testing.T) {
	w := newTestWorker(&config.Config{})
	findings := `[
		{"severity":"critical","title":"High replication lag","category":"replication","object_identifier":"standby1","occurrence_count":3},
		{"severity":"warning","title":"Unused index","category":"indexes","object_identifier":"idx_foo","occurrence_count":10},
		{"severity":"info","title":"Table stats stale","category":"vacuum","object_identifier":null,"occurrence_count":1}
	]`
	system := `{"db_size":"500 MB","connections":12,"active":3}`
	actions := "[]"

	result := w.buildStructured(findings, system, actions)

	if !strings.Contains(result, "1 critical, 1 warning, 1 info") {
		t.Errorf("expected correct severity counts, got:\n%s", result)
	}
	if !strings.Contains(result, "High replication lag") {
		t.Error("expected critical finding title in output")
	}
	if !strings.Contains(result, "(`standby1`)") {
		t.Error("expected object identifier in output")
	}
	if !strings.Contains(result, "Unused index") {
		t.Error("expected warning finding title in output")
	}
}

func TestBuildStructured_WithActions(t *testing.T) {
	w := newTestWorker(&config.Config{})
	actions := `[
		{"action_type":"create_index","outcome":"success","executed_at":"2025-01-01T00:00:00Z"},
		{"action_type":"vacuum","outcome":"failure","executed_at":"2025-01-01T01:00:00Z"}
	]`

	result := w.buildStructured("[]", `{}`, actions)

	if !strings.Contains(result, "## Recent Actions (24h)") {
		t.Error("expected recent actions section")
	}
	if !strings.Contains(result, "create_index") {
		t.Error("expected action type in output")
	}
	if !strings.Contains(result, "success") {
		t.Error("expected action outcome in output")
	}
}

func TestBuildStructured_MalformedJSON(t *testing.T) {
	w := newTestWorker(&config.Config{})
	// Should not panic on invalid JSON; just skip the section.
	result := w.buildStructured("not-json", "not-json", "not-json")
	if !strings.Contains(result, "# pg_sage Health Briefing") {
		t.Error("header should always be present even with bad JSON")
	}
}

func TestBuildStructured_FindingSeverityIcons(t *testing.T) {
	w := newTestWorker(&config.Config{})

	tests := []struct {
		severity string
		icon     string
	}{
		{"critical", "\xf0\x9f\x94\xb4"}, // red circle
		{"warning", "\xf0\x9f\x9f\xa1"},  // yellow circle
		{"info", "\xe2\x84\xb9"},          // info
	}

	for _, tt := range tests {
		finding := []map[string]any{
			{"severity": tt.severity, "title": "test", "category": "test"},
		}
		data, _ := json.Marshal(finding)
		result := w.buildStructured(string(data), `{}`, "[]")
		if !strings.Contains(result, tt.icon) {
			t.Errorf("severity %q: expected icon in output", tt.severity)
		}
	}
}

func TestBuildStructured_NilObjectIdentifier(t *testing.T) {
	w := newTestWorker(&config.Config{})
	findings := `[{"severity":"warning","title":"test finding","object_identifier":null}]`
	result := w.buildStructured(findings, `{}`, "[]")

	// Should not contain backtick-wrapped nil.
	if strings.Contains(result, "(`<nil>`)") {
		t.Error("nil object_identifier should not appear in output")
	}
	if strings.Contains(result, "(``)") {
		t.Error("empty object_identifier should not appear in output")
	}
}

func TestBuildStructured_SystemOverviewKeys(t *testing.T) {
	w := newTestWorker(&config.Config{})
	system := `{"db_size":"1 GB","connections":20,"active":5,"cache_hit_ratio":99.5,"uptime_hours":48}`
	result := w.buildStructured("[]", system, "[]")

	for _, key := range []string{"db_size", "connections", "active", "cache_hit_ratio", "uptime_hours"} {
		if !strings.Contains(result, key) {
			t.Errorf("expected system key %q in overview", key)
		}
	}
}

func TestNew(t *testing.T) {
	cfg := &config.Config{}
	w := New(nil, cfg, nil, noopLog)
	if w == nil {
		t.Fatal("expected non-nil Worker")
	}
	if w.cfg != cfg {
		t.Error("config not stored correctly")
	}
	if w.pool != nil {
		t.Error("pool should be nil")
	}
}

func TestDispatch_Stdout(t *testing.T) {
	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			Channels: []string{"stdout"},
		},
	}
	w := newTestWorker(cfg)
	// Should not panic.
	w.Dispatch("test briefing")
}

func TestDispatch_SlackWithoutURL(t *testing.T) {
	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			Channels:        []string{"slack"},
			SlackWebhookURL: "",
		},
	}
	w := newTestWorker(cfg)
	// Should not panic; slack is skipped when URL is empty.
	w.Dispatch("test briefing")
}

func TestDispatch_EmptyChannels(t *testing.T) {
	cfg := &config.Config{
		Briefing: config.BriefingConfig{
			Channels: []string{},
		},
	}
	w := newTestWorker(cfg)
	w.Dispatch("test briefing")
}

func TestParseScheduleHour(t *testing.T) {
	tests := []struct {
		cron string
		want int
	}{
		{"0 6 * * *", 6},
		{"0 0 * * *", 0},
		{"0 23 * * *", 23},
		{"30 14 * * 1-5", 14},
		{"", -1},
		{"0", -1},
		{"0 25 * * *", -1},   // hour > 23
		{"0 abc * * *", -1},  // non-numeric
	}
	for _, tt := range tests {
		got := parseScheduleHour(tt.cron)
		if got != tt.want {
			t.Errorf("parseScheduleHour(%q) = %d, want %d",
				tt.cron, got, tt.want)
		}
	}
}

func TestShouldRun_FirstRunPastHour(t *testing.T) {
	w := &Worker{scheduleHour: 6}
	now := time.Date(2026, 3, 27, 7, 0, 0, 0, time.UTC)
	if !w.ShouldRun(now) {
		t.Error("should run: first run, past scheduled hour")
	}
}

func TestShouldRun_FirstRunBeforeHour(t *testing.T) {
	w := &Worker{scheduleHour: 6}
	now := time.Date(2026, 3, 27, 5, 0, 0, 0, time.UTC)
	if w.ShouldRun(now) {
		t.Error("should not run: first run, before scheduled hour")
	}
}

func TestShouldRun_AlreadyRanToday(t *testing.T) {
	w := &Worker{
		scheduleHour: 6,
		lastRun:      time.Date(2026, 3, 27, 6, 5, 0, 0, time.UTC),
	}
	now := time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC)
	if w.ShouldRun(now) {
		t.Error("should not run: already ran today")
	}
}

func TestShouldRun_NewDayPastHour(t *testing.T) {
	w := &Worker{
		scheduleHour: 6,
		lastRun:      time.Date(2026, 3, 26, 6, 5, 0, 0, time.UTC),
	}
	now := time.Date(2026, 3, 27, 7, 0, 0, 0, time.UTC)
	if !w.ShouldRun(now) {
		t.Error("should run: new day, past scheduled hour")
	}
}

func TestShouldRun_InvalidSchedule(t *testing.T) {
	w := &Worker{scheduleHour: -1}
	now := time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC)
	if w.ShouldRun(now) {
		t.Error("should not run with invalid schedule")
	}
}

func TestMarkRan(t *testing.T) {
	w := &Worker{scheduleHour: 6}
	if !w.lastRun.IsZero() {
		t.Error("lastRun should be zero initially")
	}
	w.MarkRan()
	if w.lastRun.IsZero() {
		t.Error("lastRun should be set after MarkRan")
	}
}

func TestGenerate_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)

	// Insert a test finding.
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.findings
		 (category, severity, object_identifier, title, detail,
		  status, last_seen, occurrence_count)
		 VALUES ('test_category', 'warning', 'public.test_table',
		         'Test finding for briefing', '{}'::jsonb,
		         'open', now(), 1)`)
	if err != nil {
		t.Fatalf("inserting test finding: %v", err)
	}

	// Cleanup after test.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM sage.findings
			 WHERE category='test_category'
			   AND title='Test finding for briefing'`)
	})

	cfg := &config.Config{}
	w := New(pool, cfg, nil, noopLog)

	// Generate uses the DB to gather findings, system info, and actions.
	output, err := w.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(output, "Test finding for briefing") {
		t.Error("briefing output should contain the test finding title")
	}
	if !strings.Contains(output, "# pg_sage Health Briefing") {
		t.Error("briefing output should contain the header")
	}
	if !strings.Contains(output, "warning") {
		t.Error("briefing output should contain severity level")
	}
}
