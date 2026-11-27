package alerting

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alerting-07 on real PostgreSQL: after a restart the throttle is seeded
// from sage.alert_log far enough back to honour the longest repeat
// interval, with the first send of each open episode, so a restart never
// turns a reminder schedule back into a fresh alert.
//
// Invalid input: not applicable; every row is written by pg_sage itself.

// repeatFinding inserts a finding with an explicit identity and status
// and removes it, with its alert_log rows, when the test ends.
func repeatFinding(
	t *testing.T, pool *pgxpool.Pool, sev, ident, status string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO sage.findings (category, severity, title, status,
		   last_seen, object_identifier, detail, resolved_at)
		 VALUES ('repeat07', $1, $2, $3, now(), $2, '{}',
		   CASE WHEN $3 = 'resolved' THEN now() END)
		 RETURNING id`, sev, ident, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert %s finding %s: %v", status, ident, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM sage.alert_log WHERE finding_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE id = $1`, id)
	})
	return id
}

// logSent writes a delivered alert_log row at an explicit time.
func logSent(t *testing.T, pool *pgxpool.Pool, id int64, sev, key string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.alert_log
		(finding_id, severity, channel, dedup_key, status, error_message, sent_at)
		VALUES ($1, $2, 'slack', $3, 'sent', '', $4)`, id, sev, key, at); err != nil {
		t.Fatalf("insert alert_log row for %d: %v", id, err)
	}
}

func repeatIdent(tag string) string {
	return fmt.Sprintf("%s_%d", tag, time.Now().UnixNano())
}

// restarted builds a fresh Manager, seeds it and pins its clock at base.
func restarted(t *testing.T, pool *pgxpool.Pool, base time.Time) (*Manager, *repeatClock) {
	t.Helper()
	m := New(pool, ManagerConfig{}, nil, noop)
	m.seedThrottle(context.Background())
	clock := &repeatClock{at: base}
	m.throttle.now = clock.now
	return m, clock
}

func setAt(c *repeatClock, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// Restart after a reminder at T: nothing before T+240m, then a reminder
// whose open-for counts from the first send of the episode.
func TestSeed_RestartAfterReminderWaitsFullInterval(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	ident := repeatIdent("restart")
	key := FormatDedupKey("repeat07", ident)
	id := repeatFinding(t, pool, "critical", ident, "open")
	logSent(t, pool, id, "critical", key, base.Add(-6*time.Hour))
	logSent(t, pool, id, "critical", key, base.Add(-2*time.Hour))

	m, clock := restarted(t, pool, base)
	wantDecision(t, "right after restart",
		first(m.throttle.DecideEpisode(key, "critical", id)), DecisionThrottled)
	setAt(clock, base.Add(2*time.Hour-time.Second))
	wantDecision(t, "T+239m59s",
		first(m.throttle.DecideEpisode(key, "critical", id)), DecisionThrottled)
	setAt(clock, base.Add(2*time.Hour))
	d, openFor := m.throttle.DecideEpisode(key, "critical", id)
	wantDecision(t, "T+240m", d, DecisionRemind)
	if openFor != 8*time.Hour {
		t.Errorf("openFor = %v, want 8h from the first send", openFor)
	}
}

// CHECK-17: with max(repeat) at 24 h the seed reads rows older than 6 h.
func TestSeed_CHECK17_ReadsRowsOlderThanSixHours(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	ident := repeatIdent("check17")
	key := FormatDedupKey("repeat07", ident)
	id := repeatFinding(t, pool, "warning", ident, "open")
	logSent(t, pool, id, "warning", key, base.Add(-20*time.Hour))

	m, clock := restarted(t, pool, base)
	wantDecision(t, "20h after the warning was sent",
		first(m.throttle.DecideEpisode(key, "warning", id)), DecisionThrottled)
	setAt(clock, base.Add(4*time.Hour))
	wantDecision(t, "24h after the warning was sent",
		first(m.throttle.DecideEpisode(key, "warning", id)), DecisionRemind)
}

// The window is max(repeat) + 1 h for findings no longer open: a resolved
// finding's send 20 h ago is seeded (cooldown flap protection), 26 h ago
// is not.
func TestSeed_WindowBoundsResolvedFindings(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	inside, outside := repeatIdent("inside"), repeatIdent("outside")
	idIn := repeatFinding(t, pool, "warning", inside, "resolved")
	idOut := repeatFinding(t, pool, "warning", outside, "resolved")
	logSent(t, pool, idIn, "warning", FormatDedupKey("repeat07", inside),
		base.Add(-20*time.Hour))
	logSent(t, pool, idOut, "warning", FormatDedupKey("repeat07", outside),
		base.Add(-26*time.Hour))

	m, _ := restarted(t, pool, base)
	if _, ok := m.throttle.sent[FormatDedupKey("repeat07", inside)]; !ok {
		t.Error("resolved finding sent 20h ago was not seeded (window < 25h)")
	}
	if _, ok := m.throttle.sent[FormatDedupKey("repeat07", outside)]; ok {
		t.Error("resolved finding sent 26h ago was seeded (window > 25h)")
	}
}

// An open finding is seeded however old its last send, so info with
// repeat 0 stays silent across a restart three days in.
func TestSeed_OpenFindingOlderThanWindowStaysSilent(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	ident := repeatIdent("oldinfo")
	key := FormatDedupKey("repeat07", ident)
	id := repeatFinding(t, pool, "info", ident, "open")
	logSent(t, pool, id, "info", key, base.Add(-72*time.Hour))

	m, _ := restarted(t, pool, base)
	wantDecision(t, "info open for three days",
		first(m.throttle.DecideEpisode(key, "info", id)), DecisionThrottled)
}

// first is the earliest send of the open episode, not of an older,
// resolved finding with the same identity.
func TestSeed_FirstIsEarliestSendOfTheOpenEpisode(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	ident := repeatIdent("episodes")
	key := FormatDedupKey("repeat07", ident)
	old := repeatFinding(t, pool, "critical", ident, "resolved")
	logSent(t, pool, old, "critical", key, base.Add(-10*time.Hour))
	cur := repeatFinding(t, pool, "critical", ident, "open")
	logSent(t, pool, cur, "critical", key, base.Add(-5*time.Hour))
	logSent(t, pool, cur, "critical", key, base.Add(-270*time.Minute))

	m, _ := restarted(t, pool, base)
	d, openFor := m.throttle.DecideEpisode(key, "critical", cur)
	wantDecision(t, "4h30m after the last send", d, DecisionRemind)
	if openFor != 5*time.Hour {
		t.Errorf("openFor = %v, want 5h from the open episode's first send", openFor)
	}
}

// A reminder sent through evaluate logs as 'sent' with an empty error,
// like any delivery, and reaches the channel marked as a reminder.
func TestEvaluate_ReminderLogsAsSent(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	ident := repeatIdent("evaluate")
	key := FormatDedupKey("repeat07", ident)
	id := repeatFinding(t, pool, "critical", ident, "open")
	logSent(t, pool, id, "critical", key, base.Add(-241*time.Minute))
	ch := &scriptedChannel{}
	m := New(pool, ManagerConfig{}, map[string][]Channel{"critical": {ch}}, noop)
	m.lastCheck = base.Add(-time.Second)
	if err := m.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ch.sentFor(id) != 1 {
		t.Fatalf("finding %d sent %d times, want one reminder", id, ch.sentFor(id))
	}
	a := ch.alerts[0]
	if !a.Reminder || a.OpenFor < 240*time.Minute || a.OpenFor > 250*time.Minute {
		t.Errorf("alert reminder %v openFor %v, want a reminder open about 4h1m",
			a.Reminder, a.OpenFor)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.alert_log
		WHERE finding_id = $1 AND status = 'sent' AND channel = 'scripted'
		  AND error_message = ''`, id).Scan(&n); err != nil {
		t.Fatalf("count reminder rows: %v", err)
	}
	if n != 1 {
		t.Errorf("reminder alert_log rows = %d, want 1 'sent' row with empty error", n)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE sage.findings SET last_seen = clock_timestamp() WHERE id = $1`, id); err != nil {
		t.Fatalf("observe finding again: %v", err)
	}
	if err := m.evaluate(context.Background()); err != nil {
		t.Fatalf("second evaluate: %v", err)
	}
	if ch.sentFor(id) != 1 {
		t.Errorf("finding %d sent again on the next cycle", id)
	}
}

// logResolved writes a resolve notification row at an explicit time.
func logResolved(t *testing.T, pool *pgxpool.Pool, id int64, key string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.alert_log
		(finding_id, severity, channel, dedup_key, status, error_message, sent_at)
		VALUES ($1, 'critical', 'slack', $2, 'resolved', '', $3)`, id, key, at); err != nil {
		t.Fatalf("insert resolved alert_log row for %d: %v", id, err)
	}
}

// A row an action resolved can reopen with the same id. A resolve logged
// after the last send ends that episode across a restart, so the reopened
// finding is alerted again instead of waiting for the next reminder.
func TestSeed_ResolveAfterLastSendEndsEpisode(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	ident := repeatIdent("reopened")
	key := FormatDedupKey("repeat07", ident)
	id := repeatFinding(t, pool, "critical", ident, "open")
	logSent(t, pool, id, "critical", key, base.Add(-time.Hour))
	logResolved(t, pool, id, key, base.Add(-30*time.Minute))

	m, _ := restarted(t, pool, base)
	d, openFor := m.throttle.DecideEpisode(key, "critical", id)
	wantDecision(t, "reopened 30m after its resolve", d, DecisionSend)
	if openFor != 0 {
		t.Errorf("openFor = %v, want 0 for a first alert", openFor)
	}
}

// After a resolve and a new send of the same row, first counts from the
// send after the resolve, not from the earlier episode.
func TestSeed_FirstCountsFromTheLastResolve(t *testing.T) {
	pool := alertTestPool(t)
	base := dbNow(t, pool)
	ident := repeatIdent("reepisode")
	key := FormatDedupKey("repeat07", ident)
	id := repeatFinding(t, pool, "critical", ident, "open")
	logSent(t, pool, id, "critical", key, base.Add(-6*time.Hour))
	logResolved(t, pool, id, key, base.Add(-5*time.Hour))
	logSent(t, pool, id, "critical", key, base.Add(-270*time.Minute))

	m, _ := restarted(t, pool, base)
	d, openFor := m.throttle.DecideEpisode(key, "critical", id)
	wantDecision(t, "4h30m after the send that followed the resolve", d, DecisionRemind)
	if openFor != 270*time.Minute {
		t.Errorf("openFor = %v, want 4h30m from the send after the resolve", openFor)
	}
}
