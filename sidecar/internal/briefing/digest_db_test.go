package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// ux-02b on real PostgreSQL: a scheduled run builds the digest from the
// seeded ledger, sends one digest_ready event through Notifications and
// records each channel's outcome; an unchanged digest is stored, not sent.

type recordingNotifier struct {
	mu      sync.Mutex
	events  []notify.Event
	ensured []string
	result  []notify.ChannelOutcome
	err     error
}

func (r *recordingNotifier) SubmitWithOutcomes(_ context.Context, e notify.Event) (
	notify.DispatchResult, []notify.ChannelOutcome, error,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return notify.DispatchResult{Delivered: len(r.result)}, r.result, r.err
}

func (r *recordingNotifier) EnsureDefaultRule(_ context.Context, event string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensured = append(r.ensured, event)
	return false, nil
}

func (r *recordingNotifier) sent() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

func sentTo(name string) []notify.ChannelOutcome {
	return []notify.ChannelOutcome{{ChannelID: 1, Channel: name, Status: notify.OutcomeSent}}
}

func digestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "digest"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// seedSixChanges writes 6 changes in the last hour: 5 judged improved and
// one rolled back, plus two held-back decisions.
func seedSixChanges(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.action_log (executed_at, action_type, sql_executed,
		rollback_sql, outcome, toil_minutes_saved)
		SELECT now() - interval '30 minutes', 'create_index',
		       'CREATE INDEX CONCURRENTLY i' || g || ' ON t (a)', 'DROP INDEX i' || g,
		       CASE WHEN g = 6 THEN 'rolled_back' ELSE 'success' END, 30
		FROM generate_series(1, 6) g`)
	mustExec(t, pool, `INSERT INTO sage.action_outcome (action_log_id, action_class,
		predicted, prediction_method, verdict, observed)
		SELECT id, 'index_create', '{"class":"index_create","method":"hypopg"}', 'hypopg',
		       'improved', '{"metric":"blocks_per_call","change_pct":-40}'
		FROM sage.action_log WHERE outcome = 'success'`)
	mustExec(t, pool, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier,
		reason, evidence_id, fingerprint) VALUES
		('index', 'index', 'parked', 'safe', 'outside_maintenance_window', 'ev-a', 'fp-a'),
		('index', 'index', 'parked', 'safe', 'outside_maintenance_window', 'ev-b', 'fp-b')`)
}

func digestWorker(pool *pgxpool.Pool, n Notifier, onlyChanged bool, inboxTotal int) *Worker {
	cfg := &config.Config{Briefing: config.BriefingConfig{Schedule: "0 6 * * *",
		OnlyWhenChanged: onlyChanged}}
	counter := func(context.Context, string) (InboxCounts, error) {
		by := map[string]int{}
		if inboxTotal > 0 {
			by["change_approval"] = inboxTotal
		}
		return InboxCounts{Total: inboxTotal, ByKind: by}, nil
	}
	return New(pool, cfg, nil, noopLog).WithDatabaseName("prod").WithNotifier(n).
		WithInboxCounter(counter)
}

type storedDigest struct {
	hash     *string
	digest   Digest
	delivery map[string]Delivery
}

func latestDigest(t *testing.T, pool *pgxpool.Pool) storedDigest {
	t.Helper()
	var raw, status []byte
	var out storedDigest
	err := pool.QueryRow(context.Background(), `SELECT digest_hash,
		content_json->'digest', COALESCE(delivery_status, '{}'::jsonb)
		FROM sage.briefings ORDER BY id DESC LIMIT 1`).Scan(&out.hash, &raw, &status)
	if err != nil {
		t.Fatalf("read latest briefing: %v", err)
	}
	if err := json.Unmarshal(raw, &out.digest); err != nil {
		t.Fatalf("decode digest %s: %v", raw, err)
	}
	if err := json.Unmarshal(status, &out.delivery); err != nil {
		t.Fatalf("decode delivery %s: %v", status, err)
	}
	return out
}

func TestDigestDB_SeededWindowSendsOneDigestReady(t *testing.T) {
	pool := digestDB(t)
	seedSixChanges(t, pool)
	n := &recordingNotifier{result: sentTo("team-slack")}
	if err := digestWorker(pool, n, true, 2).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := n.sent()
	if len(events) != 1 {
		t.Fatalf("%d events, want one digest_ready", len(events))
	}
	e := events[0]
	if e.Type != notify.EventDigestReady || e.Severity != "info" {
		t.Fatalf("event = %s/%s, want digest_ready/info", e.Type, e.Severity)
	}
	head := "6 changes, 5 verified, 1 reverted, 2 need you"
	if !strings.Contains(e.Subject, head) || !strings.Contains(e.Body, head) {
		t.Fatalf("event subject %q / body %q lack %q", e.Subject, e.Body, head)
	}
	if e.Data["database"] != "prod" {
		t.Fatalf("event database = %v, want prod", e.Data["database"])
	}
	got := latestDigest(t, pool)
	var keys []string
	for _, s := range got.digest.Sections {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != "did,verified,saved,needs_you,could_not" {
		t.Fatalf("stored sections = %v", keys)
	}
	if got.digest.Counts.SavedHours != 3 {
		t.Fatalf("saved hours = %v, want 3 (6 x 30 min)", got.digest.Counts.SavedHours)
	}
	if got.hash == nil || *got.hash != got.digest.Hash || len(*got.hash) != 64 {
		t.Fatalf("digest_hash = %v, want the digest's hash %s", got.hash, got.digest.Hash)
	}
	if d := got.delivery["team-slack"]; d.Status != DeliverySent {
		t.Fatalf("delivery = %+v, want team-slack sent", got.delivery)
	}
	if strings.Join(n.ensured, ",") != notify.EventDigestReady {
		t.Fatalf("default rule ensured for %v, want digest_ready once", n.ensured)
	}
}

// An unchanged digest is stored and not sent; a changed one is sent. The
// changes are left out of the seed: each run's window starts where the
// last one ended, so a change is in one digest only. The standing state
// (what needs a person, what is held back) is what repeats.
func TestDigestDB_UnchangedIsStoredNotSent(t *testing.T) {
	pool := digestDB(t)
	mustExec(t, pool, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier,
		reason, evidence_id, fingerprint) VALUES
		('index', 'index', 'parked', 'safe', 'outside_maintenance_window', 'ev-a', 'fp-a')`)
	n := &recordingNotifier{result: sentTo("team-slack")}
	w := digestWorker(pool, n, true, 2)
	for range 2 {
		if err := w.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if len(n.sent()) != 1 {
		t.Fatalf("%d events for two equal digests, want 1", len(n.sent()))
	}
	got := latestDigest(t, pool)
	if got.delivery["digest"].Status != DeliverySuppressed {
		t.Fatalf("second delivery = %+v, want suppressed_unchanged", got.delivery)
	}
	var rows int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.briefings
		WHERE digest_hash = $1`, got.digest.Hash).Scan(&rows)
	if rows != 2 {
		t.Fatalf("%d stored rows with the hash, want both stored", rows)
	}
	changed := digestWorker(pool, n, true, 3)
	if err := changed.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(n.sent()) != 2 {
		t.Fatalf("%d events after a changed count, want 2", len(n.sent()))
	}
}

// Nil/empty: an empty digest is not sent under "only when changed", and is
// sent saying "Nothing changed" with the setting off.
func TestDigestDB_EmptyDigest(t *testing.T) {
	pool := digestDB(t)
	n := &recordingNotifier{result: sentTo("team-slack")}
	if err := digestWorker(pool, n, true, 0).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(n.sent()) != 0 {
		t.Fatalf("an empty digest was sent: %+v", n.sent())
	}
	if got := latestDigest(t, pool); got.delivery["digest"].Status != DeliverySuppressed {
		t.Fatalf("delivery = %+v, want suppressed_unchanged", got.delivery)
	}
	if err := digestWorker(pool, n, false, 0).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := n.sent()
	if len(events) != 1 || !strings.Contains(events[0].Body, "Nothing changed") {
		t.Fatalf("events = %+v, want one saying Nothing changed", events)
	}
}

// Error propagation: a failing sender is logged per channel in
// sage.notification_log and the digest row names the channel that failed.
func TestDigestDB_FailingChannelIsNamed(t *testing.T) {
	pool := digestDB(t)
	seedSixChanges(t, pool)
	var okID, badID int
	for name, typ := range map[string]string{"team-slack": "slack", "ops-hook": "webhook"} {
		var id int
		if err := pool.QueryRow(context.Background(), `INSERT INTO sage.notification_channels
			(name, type, config, enabled) VALUES ($1, $2, '{"webhook_url":"https://x.test/h"}',
			true) RETURNING id`, name, typ).Scan(&id); err != nil {
			t.Fatalf("insert channel: %v", err)
		}
		mustExec(t, pool, `INSERT INTO sage.notification_rules (channel_id, event,
			min_severity, enabled) VALUES ($1, 'digest_ready', 'info', true)`, id)
		if typ == "slack" {
			okID = id
		} else {
			badID = id
		}
	}
	store := notify.NewPoolStore(pool, nil)
	d := notify.NewDispatcherWithStore(store, noopLog)
	d.RegisterSender(okSender{})
	d.RegisterSender(badSender{})
	w := digestWorker(pool, dispatcherNotifier{d: d, s: store}, true, 2)
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := latestDigest(t, pool)
	if got.delivery["team-slack"].Status != DeliverySent ||
		got.delivery["ops-hook"].Status != DeliveryError ||
		!strings.Contains(got.delivery["ops-hook"].Error, "HTTP 500") {
		t.Fatalf("delivery = %+v, want team-slack sent and ops-hook failed", got.delivery)
	}
	var logged string
	if err := pool.QueryRow(context.Background(), `SELECT string_agg(channel_id::text ||
		':' || status, ',' ORDER BY channel_id) FROM sage.notification_log
		WHERE event = 'digest_ready'`).Scan(&logged); err != nil {
		t.Fatalf("read notification_log: %v", err)
	}
	want := map[bool]string{true: strconv.Itoa(okID) + ":sent," + strconv.Itoa(badID) + ":error",
		false: strconv.Itoa(badID) + ":error," + strconv.Itoa(okID) + ":sent"}[okID < badID]
	if logged != want {
		t.Fatalf("notification_log = %s, want %s", logged, want)
	}
}

// Error propagation: a notifier that cannot load its rules is recorded on
// the row, and the run does not fail.
func TestDigestDB_NotifierErrorIsRecorded(t *testing.T) {
	pool := digestDB(t)
	seedSixChanges(t, pool)
	n := &recordingNotifier{err: errors.New("loading rules: connection refused")}
	if err := digestWorker(pool, n, true, 2).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := latestDigest(t, pool)
	if d := got.delivery["notifications"]; d.Status != DeliveryError ||
		!strings.Contains(d.Error, "connection refused") {
		t.Fatalf("delivery = %+v, want the notifier error recorded", got.delivery)
	}
	var claimed int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.digest_delivery`).Scan(&claimed)
	if claimed != 0 {
		t.Fatal("a digest that reached no channel was marked delivered: it would never resend")
	}
}

// Error propagation (review fix): stdout taking the digest does not mark
// it delivered when every Notifications channel failed, so the unchanged
// digest is sent again at the next schedule instead of never reaching a
// person. One channel that took it does mark it delivered. Only standing
// state is seeded, so every run builds the same digest.
func TestDigestDB_StdoutAloneDoesNotMarkDelivered(t *testing.T) {
	pool := digestDB(t)
	mustExec(t, pool, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier,
		reason, evidence_id, fingerprint) VALUES
		('index', 'index', 'parked', 'safe', 'outside_maintenance_window', 'ev-a', 'fp-a')`)
	failed := &recordingNotifier{result: []notify.ChannelOutcome{{ChannelID: 1,
		Channel: "team-slack", Status: notify.OutcomeError, Error: "HTTP 500"}}}
	w := digestWorker(pool, failed, true, 2)
	w.stdout = &strings.Builder{}
	w.live.Load().cfg.Channels = []string{"stdout"}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := latestDigest(t, pool)
	if got.delivery["stdout"].Status != DeliverySent ||
		got.delivery["team-slack"].Status != DeliveryError {
		t.Fatalf("delivery = %+v, want stdout sent and team-slack failed", got.delivery)
	}
	var claimed int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.digest_delivery`).Scan(&claimed)
	if claimed != 0 {
		t.Fatalf("%d claims: a digest every notification channel missed was kept "+
			"as delivered", claimed)
	}
	failed.result = sentTo("team-slack")
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed.sent()) != 2 {
		t.Fatalf("%d events, want the unchanged digest sent again", len(failed.sent()))
	}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(failed.sent()) != 2 {
		t.Fatalf("%d events, want the delivered digest held back", len(failed.sent()))
	}
}

// Concurrency: two leader-election flaps producing the same digest send it
// once; the hash check is one INSERT ... ON CONFLICT on scope and hash.
func TestDigestDB_TwoFlapsSendOnce(t *testing.T) {
	pool := digestDB(t)
	seedSixChanges(t, pool)
	a := &recordingNotifier{result: sentTo("team-slack")}
	b := &recordingNotifier{result: sentTo("team-slack")}
	workers := []*Worker{digestWorker(pool, a, true, 2), digestWorker(pool, b, true, 2)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, w := range workers {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			<-start
			if err := w.Run(context.Background()); err != nil {
				t.Errorf("Run: %v", err)
			}
		}(w)
	}
	close(start)
	wg.Wait()
	if total := len(a.sent()) + len(b.sent()); total != 1 {
		t.Fatalf("%d digests sent by two flapping leaders, want 1", total)
	}
	var rows int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.briefings`).Scan(&rows)
	if rows != 2 {
		t.Fatalf("%d briefing rows, want both stored", rows)
	}
}

// State transition: without a notifier the digest still goes to stdout
// and the row says Notifications was not wired.
func TestDigestDB_WithoutNotifier(t *testing.T) {
	pool := digestDB(t)
	seedSixChanges(t, pool)
	var out strings.Builder
	w := digestWorker(pool, nil, true, 2)
	w.stdout = &out
	w.live.Load().cfg.Channels = []string{"stdout"}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "6 changes, 5 verified") {
		t.Fatalf("stdout = %q, want the digest", out.String())
	}
	got := latestDigest(t, pool)
	if got.delivery["stdout"].Status != DeliverySent ||
		got.delivery["notifications"].Status != DeliverySkipped {
		t.Fatalf("delivery = %+v, want stdout sent, notifications skipped", got.delivery)
	}
}

type okSender struct{}

func (okSender) Type() string                                          { return "slack" }
func (okSender) Send(context.Context, notify.Channel, notify.Event) error { return nil }

type badSender struct{}

func (badSender) Type() string { return "webhook" }
func (badSender) Send(context.Context, notify.Channel, notify.Event) error {
	return errors.New("webhook answered HTTP 500")
}

type dispatcherNotifier struct {
	d *notify.Dispatcher
	s *notify.PoolStore
}

func (n dispatcherNotifier) SubmitWithOutcomes(ctx context.Context, e notify.Event) (
	notify.DispatchResult, []notify.ChannelOutcome, error,
) {
	return n.d.SubmitWithOutcomes(ctx, e)
}

func (n dispatcherNotifier) EnsureDefaultRule(ctx context.Context, event string) (
	bool, error,
) {
	return n.s.EnsureDefaultRule(ctx, event)
}

