package briefing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// llm-06 fleet roll-up: each database stores its own briefing; one
// message covers them all and its status is written to every row. Since
// ux-02b the message is the digest, sent as one digest_ready event through
// Notifications (the briefing's own Slack post path is gone), so the
// recording notifier stands where the webhook recorder stood.

func clearDigestClaims(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := requireDB(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.digest_delivery`); err != nil {
		t.Fatalf("clear digest claims: %v", err)
	}
}

func rollupWorkers(t *testing.T, n Notifier, names ...string) []*Worker {
	t.Helper()
	pool, _ := requireDB(t)
	cfg := &config.Config{Briefing: config.BriefingConfig{Schedule: "* * * * *",
		Channels: []string{"slack"}, SlackWebhookURL: "https://hooks.example.test/x",
		OnlyWhenChanged: true}}
	out := make([]*Worker, 0, len(names))
	for _, name := range names {
		out = append(out, New(pool, cfg, nil, noopLog).WithDatabaseName(name).WithNotifier(n))
	}
	return out
}

// CHECK-15: three databases make exactly one digest event, and all three
// rows record its status.
func TestRollupSendsOneMessageForThreeDatabases(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	clearDigestClaims(t, ctx)
	n := &recordingNotifier{result: sentTo(notify.BriefingChannelName)}
	workers := rollupWorkers(t, n, "db_c", "db_a", "db_b")

	report := RunRollup(ctx, workers, time.Now().Add(2*time.Minute))
	calls := n.sent()
	if len(calls) != 1 {
		t.Fatalf("digest events = %d, want exactly 1", len(calls))
	}
	if got := strings.Join(report.Covered, ","); got != "db_a,db_b,db_c" {
		t.Fatalf("covered = %s, want db_a,db_b,db_c in name order", got)
	}
	for _, name := range []string{"db_a", "db_b", "db_c"} {
		if !strings.Contains(calls[0].Body, "## "+name) {
			t.Errorf("rollup message has no section for %s: %s", name, calls[0].Body)
		}
	}
	if calls[0].Data["database"] != "all" {
		t.Errorf("rollup event database = %v, want all", calls[0].Data["database"])
	}
	assertSharedStatus(t, report.Delivery[notify.BriefingChannelName], 3)

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
	if len(again.Covered) != 0 || len(n.sent()) != 1 {
		t.Fatalf("second tick covered %v with %d events, want nothing new",
			again.Covered, len(n.sent()))
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
		if s := status[notify.BriefingChannelName]; s.Status != DeliverySent ||
			!s.At.Equal(shared.At) {
			t.Fatalf("row %d status = %+v, want the shared sent status %+v", n, s, shared)
		}
		n++
	}
	if n != want {
		t.Fatalf("briefing rows = %d, want %d (one per database)", n, want)
	}
}

// Error propagation: a failed roll-up records the error on every row.
func TestRollupRecordsSharedFailure(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	clearDigestClaims(t, ctx)
	n := &recordingNotifier{result: []notify.ChannelOutcome{{ChannelID: 1,
		Channel: notify.BriefingChannelName, Status: notify.OutcomeError,
		Error: "send failed: slack rejected the message: HTTP 403"}}}
	report := RunRollup(ctx, rollupWorkers(t, n, "db1", "db2"),
		time.Now().Add(2*time.Minute))
	if report.Delivery[notify.BriefingChannelName].Status != DeliveryError {
		t.Fatalf("report = %+v, want a slack channel error", report.Delivery)
	}
	var errored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.briefings
		WHERE delivery_status->$1->>'status' = 'error'
		  AND delivery_status->$1->>'error' LIKE '%HTTP 403%'`,
		notify.BriefingChannelName).Scan(&errored); err != nil {
		t.Fatal(err)
	}
	if errored != 2 {
		t.Fatalf("rows with the shared 403 = %d, want 2", errored)
	}
}

// Nil/empty: no workers, or none due, sends nothing.
func TestRollupWithNothingDueSendsNothing(t *testing.T) {
	n := &recordingNotifier{}
	if r := RunRollup(context.Background(), nil, time.Now()); len(r.Covered) != 0 {
		t.Fatalf("nil workers covered %v", r.Covered)
	}
	w := (&Worker{schedule: cronSchedule{}, logFn: noopLog, cfg: &config.Config{
		Briefing: config.BriefingConfig{Channels: []string{"slack"}}}}).WithNotifier(n)
	if r := RunRollup(context.Background(), []*Worker{w, nil}, time.Now()); len(r.Covered) != 0 {
		t.Fatalf("an invalid schedule was due: %v", r.Covered)
	}
	if got := len(n.sent()); got != 0 {
		t.Fatalf("digest events = %d, want 0", got)
	}
}

// The combined text puts each database's digest under its own heading,
// in order.
func TestRollupTextHasOneSectionPerDatabase(t *testing.T) {
	a := BuildDigest(DigestInput{Database: "a", Window: digestWindow, Outcomes: sixChanges()})
	b := BuildDigest(DigestInput{Database: "b", Window: digestWindow})
	text := rollupText([]rollupPart{{database: "a", digest: a}, {database: "b", digest: b}})
	ia, ib := strings.Index(text, "## a"), strings.Index(text, "## b")
	if ia < 0 || ib < ia || !strings.Contains(text, "6 changes") ||
		!strings.Contains(text, "2 databases") {
		t.Fatalf("rollup text:\n%s", text)
	}
	send := rollupSend([]rollupPart{{database: "a", digest: a}, {database: "b", digest: b}})
	if send.headline != "6 changes, 5 verified, 1 reverted" || send.scope != rollupScope ||
		send.empty || len(send.hash) != 64 {
		t.Fatalf("rollup send = %+v", send)
	}
	swapped := rollupSend([]rollupPart{{database: "b", digest: b}, {database: "a", digest: a}})
	if swapped.hash != send.hash {
		t.Fatal("the roll-up hash depends on the order of its parts")
	}
}
