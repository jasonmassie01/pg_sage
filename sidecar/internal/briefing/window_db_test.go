package briefing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// llm-06 integration: windows, content_json and delivery_status on a real
// sage.briefings table. The fixture database belongs to this package's
// process, so these tests may clear the briefings table.

func clearBriefings(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.briefings`); err != nil {
		t.Fatalf("clear briefings: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.briefings`)
		_, _ = pool.Exec(bg, `DELETE FROM sage.action_log WHERE action_type LIKE 'llm06_%'`)
	})
}

func seedAction(t *testing.T, pool *pgxpool.Pool, ctx context.Context, kind, ago string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.action_log
		(executed_at, action_type, sql_executed, outcome)
		VALUES (now() - $1::interval, $2, 'SELECT 1', 'success')`, ago, kind)
	if err != nil {
		t.Fatalf("seed action %s: %v", kind, err)
	}
}

type storedBriefing struct {
	id                        int64
	generatedAt, start, end   time.Time
	contentJSON, deliveryJSON []byte
	contentText               string
}

func latestBriefing(t *testing.T, pool *pgxpool.Pool, ctx context.Context) storedBriefing {
	t.Helper()
	var b storedBriefing
	err := pool.QueryRow(ctx, `SELECT id, generated_at, period_start, period_end,
		content_json::text, coalesce(delivery_status::text, ''), content_text
		FROM sage.briefings ORDER BY generated_at DESC, id DESC LIMIT 1`).Scan(
		&b.id, &b.generatedAt, &b.start, &b.end, &b.contentJSON, &b.deliveryJSON,
		&b.contentText)
	if err != nil {
		t.Fatalf("read latest briefing: %v", err)
	}
	return b
}

func actionTypes(t *testing.T, raw []byte) []string {
	t.Helper()
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("content_json is not a payload: %v (%s)", err, raw)
	}
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, a["action_type"].(string))
	}
	return out
}

// Happy path, the entry's failing-today test: the second briefing, 3 days
// after the first, starts at the first's generated_at and reports an
// action executed 2 days ago, not one from before the window.
func TestBriefingWindowSpansSincePreviousBriefing(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	var first time.Time
	if err := pool.QueryRow(ctx, `INSERT INTO sage.briefings (generated_at,
		period_start, period_end, mode, content_text, content_json)
		VALUES (now() - interval '3 days', now() - interval '4 days',
		        now() - interval '3 days', 'executive', 'old', '{}')
		RETURNING generated_at`).Scan(&first); err != nil {
		t.Fatalf("seed first briefing: %v", err)
	}
	seedAction(t, pool, ctx, "llm06_inside", "2 days")
	seedAction(t, pool, ctx, "llm06_before", "4 days")

	w := New(pool, &config.Config{}, nil, noopLog).WithDatabaseName("db1")
	if _, err := w.Generate(ctx); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := latestBriefing(t, pool, ctx)
	if !got.start.Equal(first) {
		t.Fatalf("period_start = %s, want the previous generated_at %s", got.start, first)
	}
	if !got.end.Equal(got.generatedAt) {
		t.Fatalf("period_end %s != generated_at %s", got.end, got.generatedAt)
	}
	types := strings.Join(actionTypes(t, got.contentJSON), ",")
	if !strings.Contains(types, "llm06_inside") || strings.Contains(types, "llm06_before") {
		t.Fatalf("actions = %s, want llm06_inside and not llm06_before", types)
	}
	if !strings.Contains(got.contentText, "## Recent Actions (3d)") {
		t.Fatalf("structured text does not name the 3-day window:\n%s", got.contentText)
	}
	var p Payload
	_ = json.Unmarshal(got.contentJSON, &p)
	if !p.Window.Start.Equal(first) || !p.Window.End.Equal(got.generatedAt) {
		t.Fatalf("content_json window = %+v, want [%s, %s]", p.Window, first, got.generatedAt)
	}

	assertNextWindow(t, w, got)
}

// assertNextWindow checks the state transition: the worker's next briefing
// starts where prev ended and reports only the action executed since.
func assertNextWindow(t *testing.T, w *Worker, got storedBriefing) {
	t.Helper()
	pool, ctx := requireDB(t)
	w.MarkRan()
	// The window runs on the sidecar's clock; a container clock can lag it,
	// so this action is stamped from the sidecar's clock, inside the window.
	inside := got.generatedAt.Add(time.Millisecond)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log
		(executed_at, action_type, sql_executed, outcome)
		VALUES ($1, 'llm06_after', 'SELECT 1', 'success')`, inside); err != nil {
		t.Fatalf("seed llm06_after: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := w.Generate(ctx); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	next := latestBriefing(t, pool, ctx)
	if !next.start.Equal(got.generatedAt) {
		t.Fatalf("next period_start = %s, want %s", next.start, got.generatedAt)
	}
	if types := actionTypes(t, next.contentJSON); len(types) != 1 || types[0] != "llm06_after" {
		t.Fatalf("next actions = %v, want only llm06_after", types)
	}
}

// CHECK-13: no row written by Generate has content_json = '{}', and the
// payload carries the window and the severity totals.
func TestBriefingContentJSONIsThePayload(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	seedGroundFinding(t, pool, ctx)
	if _, err := New(pool, &config.Config{}, nil, noopLog).Generate(ctx); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var empty int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.briefings
		WHERE content_json = '{}'::jsonb`).Scan(&empty); err != nil {
		t.Fatal(err)
	}
	if empty != 0 {
		t.Fatalf("%d briefings have content_json = '{}'", empty)
	}
	var p Payload
	if err := json.Unmarshal(latestBriefing(t, pool, ctx).contentJSON, &p); err != nil {
		t.Fatal(err)
	}
	if p.Window.Start.IsZero() || !p.Window.End.After(p.Window.Start) {
		t.Fatalf("content_json window = %+v, want a non-empty window", p.Window)
	}
	if p.OpenBySeverity["critical"] < 1 || p.TotalOpen < 1 {
		t.Fatalf("totals = %d open, by severity %v; want the seeded critical",
			p.TotalOpen, p.OpenBySeverity)
	}
}

// Error propagation: a channel's failure is stored as the row's delivery
// status, without the webhook URL. (ux-02b: the failure now comes from the
// Notifications channel the digest went through; the briefing's own Slack
// post this test drove before is deleted.)
func TestBriefingRunStoresDeliveryStatus(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	clearDigestClaims(t, ctx)
	n := &recordingNotifier{result: []notify.ChannelOutcome{{ChannelID: 4,
		Channel: notify.BriefingChannelName, Status: notify.OutcomeError,
		Error: "send failed: slack rejected the message: HTTP 500"}}}
	cfg := &config.Config{Briefing: config.BriefingConfig{Schedule: "0 6 * * *",
		Channels: []string{"slack"}, SlackWebhookURL: "https://x.test" + secretHookPath,
		OnlyWhenChanged: true}}
	w := New(pool, cfg, nil, noopLog).WithNotifier(n)
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := latestBriefing(t, pool, ctx)
	var status map[string]Delivery
	if err := json.Unmarshal(got.deliveryJSON, &status); err != nil {
		t.Fatalf("delivery_status %q: %v", got.deliveryJSON, err)
	}
	slack := status[notify.BriefingChannelName]
	if slack.Status != DeliveryError || !strings.Contains(slack.Error, "HTTP 500") {
		t.Fatalf("delivery_status = %+v, want the channel's error with HTTP 500", status)
	}
	if strings.Contains(string(got.deliveryJSON), "SECRET") {
		t.Fatalf("delivery_status leaks the webhook URL: %s", got.deliveryJSON)
	}
	if !w.lastRun.Equal(got.generatedAt) {
		t.Fatalf("Run did not mark the stored time: lastRun %s, generated_at %s",
			w.lastRun, got.generatedAt)
	}
}
