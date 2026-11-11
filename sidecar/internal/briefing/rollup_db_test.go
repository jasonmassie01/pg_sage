package briefing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-06 fleet roll-up: each database stores its own briefing; one Slack
// message covers them all and its status is written to every row.

type hookRecorder struct {
	mu     sync.Mutex
	bodies []string
	status int
}

func (h *hookRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.bodies = append(h.bodies, string(body))
	status := h.status
	h.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
	}
}

func (h *hookRecorder) calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

func rollupWorkers(t *testing.T, hookURL string, names ...string) []*Worker {
	t.Helper()
	pool, _ := requireDB(t)
	cfg := &config.Config{Briefing: config.BriefingConfig{Schedule: "* * * * *",
		Channels: []string{"slack"}, SlackWebhookURL: hookURL}}
	out := make([]*Worker, 0, len(names))
	for _, name := range names {
		out = append(out, New(pool, cfg, nil, noopLog).WithDatabaseName(name))
	}
	return out
}

// CHECK-15: three databases make exactly one webhook call, and all three
// rows record its status.
func TestRollupSendsOneMessageForThreeDatabases(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	hook := &hookRecorder{}
	srv := httptest.NewServer(hook)
	defer srv.Close()
	workers := rollupWorkers(t, srv.URL, "db_c", "db_a", "db_b")

	report := RunRollup(ctx, workers, time.Now().Add(2*time.Minute))
	calls := hook.calls()
	if len(calls) != 1 {
		t.Fatalf("webhook calls = %d, want exactly 1", len(calls))
	}
	if got := strings.Join(report.Covered, ","); got != "db_a,db_b,db_c" {
		t.Fatalf("covered = %s, want db_a,db_b,db_c in name order", got)
	}
	for _, name := range []string{"db_a", "db_b", "db_c"} {
		if !strings.Contains(calls[0], name) {
			t.Errorf("rollup message has no section for %s: %s", name, calls[0])
		}
	}
	assertSharedStatus(t, report.Delivery["slack"], 3)

	// Boundary: at the time of the briefings just stored nothing is due, and
	// nothing is sent. Every worker was marked with its stored time.
	tick := workers[0].lastRun
	for _, w := range workers {
		if w.lastRun.IsZero() {
			t.Fatalf("worker %s was not marked as run", w.database)
		}
		if w.lastRun.Before(tick) {
			tick = w.lastRun
		}
	}
	again := RunRollup(ctx, workers, tick)
	if len(again.Covered) != 0 || len(hook.calls()) != 1 {
		t.Fatalf("second tick covered %v with %d calls, want nothing new",
			again.Covered, len(hook.calls()))
	}
}

// assertSharedStatus checks that every briefing row holds the one message's
// sent status, and that there are want rows.
func assertSharedStatus(t *testing.T, shared Delivery, want int) {
	t.Helper()
	pool, ctx := requireDB(t)
	rows, err := pool.Query(ctx, `SELECT delivery_status FROM sage.briefings`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var status map[string]Delivery
		if err := json.Unmarshal(raw, &status); err != nil {
			t.Fatalf("row %d delivery_status %q: %v", n, raw, err)
		}
		if s := status["slack"]; s.Status != DeliverySent || s.HTTPStatus != http.StatusOK ||
			!s.At.Equal(shared.At) {
			t.Fatalf("row %d slack status = %+v, want the shared sent status %+v", n, s, shared)
		}
		n++
	}
	if n != want {
		t.Fatalf("briefing rows = %d, want %d (one per database)", n, want)
	}
}

// Error propagation: a rejected roll-up records the error on every row.
func TestRollupRecordsSharedFailure(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	hook := &hookRecorder{status: http.StatusForbidden}
	srv := httptest.NewServer(hook)
	defer srv.Close()
	report := RunRollup(ctx, rollupWorkers(t, srv.URL, "db1", "db2"),
		time.Now().Add(2*time.Minute))
	if report.Delivery["slack"].Status != DeliveryError {
		t.Fatalf("report = %+v, want a slack error", report.Delivery)
	}
	var errored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.briefings
		WHERE delivery_status->'slack'->>'status' = 'error'
		  AND (delivery_status->'slack'->>'http_status')::int = 403`).Scan(&errored); err != nil {
		t.Fatal(err)
	}
	if errored != 2 {
		t.Fatalf("rows with the shared 403 = %d, want 2", errored)
	}
}

// Nil/empty: no workers, or none due, sends nothing.
func TestRollupWithNothingDueSendsNothing(t *testing.T) {
	hook := &hookRecorder{}
	srv := httptest.NewServer(hook)
	defer srv.Close()
	if r := RunRollup(context.Background(), nil, time.Now()); len(r.Covered) != 0 {
		t.Fatalf("nil workers covered %v", r.Covered)
	}
	w := &Worker{schedule: cronSchedule{}, logFn: noopLog, cfg: &config.Config{
		Briefing: config.BriefingConfig{Channels: []string{"slack"},
			SlackWebhookURL: srv.URL}}}
	if r := RunRollup(context.Background(), []*Worker{w, nil}, time.Now()); len(r.Covered) != 0 {
		t.Fatalf("an invalid schedule was due: %v", r.Covered)
	}
	if n := len(hook.calls()); n != 0 {
		t.Fatalf("webhook calls = %d, want 0", n)
	}
}

// The combined text puts each database under its own heading, in order.
func TestRollupTextHasOneSectionPerDatabase(t *testing.T) {
	text := rollupText([]rollupPart{{database: "a", text: "# A\nbody a"},
		{database: "b", text: "# B\nbody b"}})
	ia, ib := strings.Index(text, "## a"), strings.Index(text, "## b")
	if ia < 0 || ib < ia || !strings.Contains(text, "body a") ||
		!strings.Contains(text, "2 databases") {
		t.Fatalf("rollup text:\n%s", text)
	}
}
