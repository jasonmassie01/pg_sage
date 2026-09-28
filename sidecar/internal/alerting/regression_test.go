package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// scriptedChannel fails the first failN sends, runs hook on each send
// and records every alert it receives.
type scriptedChannel struct {
	mu     sync.Mutex
	failN  int
	calls  int
	alerts []Alert
	hook   func()
}

func (c *scriptedChannel) Name() string { return "scripted" }
func (c *scriptedChannel) Send(_ context.Context, a Alert) error {
	c.mu.Lock()
	c.calls++
	c.alerts = append(c.alerts, a)
	n, hook := c.calls, c.hook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	if n <= c.failN {
		return errors.New("upstream 503")
	}
	return nil
}

func (c *scriptedChannel) sentFor(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, a := range c.alerts {
		for _, f := range a.Findings {
			if f.ID == id && !a.Resolved {
				n++
			}
		}
	}
	return n
}

func insertAlertFinding(
	t *testing.T, pool *pgxpool.Pool, sev, tag string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO sage.findings (category, severity, title, status,
		   last_seen, object_identifier, detail)
		 VALUES ('surf15_test', $1, $2, 'open', now(), $2, '{}')
		 RETURNING id`, sev, fmt.Sprintf("%s_%d", tag, time.Now().UnixNano()),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM sage.alert_log WHERE finding_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE id = $1`, id)
	})
	return id
}

func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatalf("clock_timestamp: %v", err)
	}
	return now
}

// TestEvaluate_RetriesFailedDelivery is the SURF-15 regression: a
// finding whose delivery failed on every channel was never retried
// because the watermark had already moved past its last_seen.
func TestEvaluate_RetriesFailedDelivery(t *testing.T) {
	pool := connectAlertTestDB(t)
	defer pool.Close()
	start := dbNow(t, pool)
	id := insertAlertFinding(t, pool, "critical", "retry")
	ch := &scriptedChannel{failN: 1}
	m := New(pool, ManagerConfig{}, map[string][]Channel{"critical": {ch}}, noop)
	m.lastCheck = start.Add(-time.Second)

	for i := 0; i < 2; i++ {
		if err := m.evaluate(context.Background()); err != nil {
			t.Fatalf("evaluate %d: %v", i, err)
		}
	}
	if got := ch.sentFor(id); got != 2 {
		t.Fatalf("send attempts for finding = %d, want 2 (fail, retry)", got)
	}
	var sent int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.alert_log
		  WHERE finding_id = $1 AND status = 'sent'`, id).Scan(&sent); err != nil {
		t.Fatalf("count sent: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent rows = %d, want 1", sent)
	}
	if err := m.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate 3: %v", err)
	}
	if got := ch.sentFor(id); got != 2 {
		t.Fatalf("delivered finding re-sent: attempts = %d", got)
	}
}

// TestEvaluate_WatermarkCapturedBeforeQuery is the G7-B18 / SURF-15
// regression: findings that appeared during a slow dispatch fell
// behind a watermark taken after dispatch.
func TestEvaluate_WatermarkCapturedBeforeQuery(t *testing.T) {
	pool := connectAlertTestDB(t)
	defer pool.Close()
	start := dbNow(t, pool)
	first := insertAlertFinding(t, pool, "critical", "wm_a")
	var second atomic.Int64
	ch := &scriptedChannel{}
	ch.hook = func() {
		if second.Load() == 0 {
			second.Store(insertAlertFinding(t, pool, "critical", "wm_b"))
			time.Sleep(20 * time.Millisecond)
		}
	}
	m := New(pool, ManagerConfig{}, map[string][]Channel{"critical": {ch}}, noop)
	m.lastCheck = start.Add(-time.Second)
	for i := 0; i < 2; i++ {
		if err := m.evaluate(context.Background()); err != nil {
			t.Fatalf("evaluate %d: %v", i, err)
		}
	}
	if ch.sentFor(first) != 1 {
		t.Fatalf("first finding sends = %d", ch.sentFor(first))
	}
	if got := ch.sentFor(second.Load()); got != 1 {
		t.Fatalf("finding created during dispatch sent %d times, want 1", got)
	}
}

// TestEvaluate_QuietHoursDeferNotDrop is the G7-B19 regression:
// non-critical alerts suppressed by quiet hours were dropped.
func TestEvaluate_QuietHoursDeferNotDrop(t *testing.T) {
	pool := connectAlertTestDB(t)
	defer pool.Close()
	start := dbNow(t, pool)
	id := insertAlertFinding(t, pool, "warning", "quiet")
	ch := &scriptedChannel{}
	m := New(pool, ManagerConfig{QuietHoursStart: "00:00",
		QuietHoursEnd: "00:00"}, map[string][]Channel{"warning": {ch}}, noop)
	m.throttle.now = func() time.Time {
		return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	}
	m.throttle.quietStart, m.throttle.quietEnd = 0, 24*60
	m.lastCheck = start.Add(-time.Second)
	if err := m.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate quiet: %v", err)
	}
	if ch.sentFor(id) != 0 {
		t.Fatal("warning sent during quiet hours")
	}
	m.throttle.quietStart, m.throttle.quietEnd = -1, -1
	if err := m.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate after quiet: %v", err)
	}
	if got := ch.sentFor(id); got != 1 {
		t.Fatalf("deferred warning delivered %d times after quiet hours, want 1", got)
	}
}

// TestThrottle_CriticalBypassesQuietHours is the other half of B19.
func TestThrottle_CriticalBypassesQuietHours(t *testing.T) {
	th := NewThrottle(0, "00:00", "23:59", "UTC")
	th.now = func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }
	if !th.ShouldAlert("k1", "critical") {
		t.Fatal("critical suppressed by quiet hours")
	}
	if th.ShouldAlert("k2", "warning") {
		t.Fatal("warning not suppressed by quiet hours")
	}
	if d := th.Decide("k2", "warning"); d != DecisionDefer {
		t.Fatalf("warning decision = %v, want DecisionDefer", d)
	}
}

// TestThrottle_QuietHoursHonourMinutes is a G7-B32 regression:
// "22:30" was treated as 22:00.
func TestThrottle_QuietHoursHonourMinutes(t *testing.T) {
	th := NewThrottle(0, "22:30", "06:15", "UTC")
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC) }
	cases := map[time.Time]bool{
		at(22, 15): false, at(22, 45): true, at(6, 0): true, at(6, 20): false,
	}
	for ts, want := range cases {
		if got := th.IsQuietHours(ts); got != want {
			t.Errorf("IsQuietHours(%s) = %v, want %v", ts.Format("15:04"), got, want)
		}
	}
}

// TestThrottle_EvictsExpiredEntries is a G7-B32 regression: the sent
// map grew forever.
func TestThrottle_EvictsExpiredEntries(t *testing.T) {
	th := NewThrottle(0, "", "", "UTC")
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	th.now = func() time.Time { return clock }
	for i := 0; i < 1000; i++ {
		th.Record(fmt.Sprintf("k%d", i), "info")
	}
	clock = clock.Add(7 * time.Hour)
	th.Record("fresh", "info")
	if n := len(th.sent); n != 1 {
		t.Fatalf("sent map size = %d after cooldown expiry, want 1", n)
	}
}

// TestNew_WarnsOnInvalidTimezone is a G7-B32 regression: an invalid
// timezone silently became UTC.
func TestNew_WarnsOnInvalidTimezone(t *testing.T) {
	var warned atomic.Bool
	logFn := func(level, msg string, args ...any) {
		if level == "WARN" && strings.Contains(fmt.Sprintf(msg, args...), "timezone") {
			warned.Store(true)
		}
	}
	New(nil, ManagerConfig{Timezone: "Mars/Olympus_Mons"}, nil, logFn)
	if !warned.Load() {
		t.Fatal("invalid timezone not reported")
	}
}

// TestSlack_NoRetryOnPermanent4xx is a G7-B32 regression: 400/403
// responses were retried three times.
func TestSlack_NoRetryOnPermanent4xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
	defer srv.Close()
	s := NewSlack(srv.URL, noop)
	if err := s.Send(context.Background(), Alert{
		Findings: []AlertFinding{sampleFinding(1, "critical")},
	}); err == nil {
		t.Fatal("expected error for 400")
	}
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (no retry on 4xx)", hits.Load())
	}
}

// TestSenders_ErrorsDoNotLeakURLs is the alerting half of G7-B10.
func TestSenders_ErrorsDoNotLeakURLs(t *testing.T) {
	secret := "http://127.0.0.1:1/services/T0/B0/ALERTSECRET"
	alert := Alert{Findings: []AlertFinding{sampleFinding(1, "critical")}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ch := range []Channel{
		NewSlack(secret, noop), NewWebhook("w", secret, nil, noop),
	} {
		err := ch.Send(ctx, alert)
		if err == nil || strings.Contains(err.Error(), "ALERTSECRET") {
			t.Fatalf("%s error = %v", ch.Name(), err)
		}
	}
}

// TestSlackPayload_LimitsAndEscaping is the alerting half of B15/B31.
func TestSlackPayload_LimitsAndEscaping(t *testing.T) {
	f := sampleFinding(1, "critical")
	f.Title = "<!channel> " + strings.Repeat("t", 4000)
	f.Recommendation = "<https://evil.example|click>"
	raw, err := NewSlack("https://hooks.slack.com/x", noop).buildPayload(
		Alert{Findings: []AlertFinding{f}, Severity: "critical",
			Database: strings.Repeat("d", 300)})
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p struct {
		Blocks []struct {
			Text struct{ Text string } `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := utf8.RuneCountInString(p.Blocks[0].Text.Text); n > 150 {
		t.Fatalf("header runes = %d", n)
	}
	sec := p.Blocks[1].Text.Text
	if utf8.RuneCountInString(sec) > 3000 || strings.Contains(sec, "<!channel>") ||
		strings.Contains(sec, "<https://evil") {
		t.Fatalf("section not bounded/escaped: %q", sec[:60])
	}
}

// TestPayloads_CarryDatabaseName is the library half of G7-B09: fleet
// managers must say which database an alert is about.
func TestPayloads_CarryDatabaseName(t *testing.T) {
	alert := Alert{Findings: []AlertFinding{sampleFinding(1, "critical")},
		Severity: "critical", Database: "billing"}
	slack, _ := NewSlack("https://hooks.slack.com/x", noop).buildPayload(alert)
	pd, _ := NewPagerDuty("rk", noop).buildPayload(alert)
	web, _ := json.Marshal(alert)
	for name, raw := range map[string][]byte{"slack": slack, "pd": pd, "webhook": web} {
		if !strings.Contains(string(raw), "billing") {
			t.Errorf("%s payload lacks database name: %s", name, raw)
		}
	}
}

// TestPagerDuty_ResolveUsesSameDedupKey is the alerting half of B14.
func TestPagerDuty_ResolveUsesSameDedupKey(t *testing.T) {
	f := sampleFinding(1, "critical")
	p := NewPagerDuty("rk", noop)
	trig, _ := p.buildPayload(Alert{Findings: []AlertFinding{f}, Severity: "critical"})
	res, _ := p.buildPayload(Alert{Findings: []AlertFinding{f},
		Severity: "critical", Resolved: true})
	var a, b map[string]any
	_ = json.Unmarshal(trig, &a)
	_ = json.Unmarshal(res, &b)
	if b["event_action"] != "resolve" || a["dedup_key"] != b["dedup_key"] {
		t.Fatalf("trigger=%v resolve=%v", a, b)
	}
}

// TestEvaluate_SendsResolveForAlertedFinding: resolved findings that
// were alerted produce exactly one resolve notification (B14).
func TestEvaluate_SendsResolveForAlertedFinding(t *testing.T) {
	pool := connectAlertTestDB(t)
	defer pool.Close()
	start := dbNow(t, pool)
	id := insertAlertFinding(t, pool, "critical", "resolve")
	ch := &scriptedChannel{}
	m := New(pool, ManagerConfig{}, map[string][]Channel{"critical": {ch}}, noop)
	m.lastCheck = start.Add(-time.Second)
	if err := m.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE sage.findings
		SET status = 'resolved', resolved_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := m.evaluate(context.Background()); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
	}
	resolves := 0
	for _, a := range ch.alerts {
		if a.Resolved && len(a.Findings) == 1 && a.Findings[0].ID == id {
			resolves++
		}
	}
	if resolves != 1 {
		t.Fatalf("resolve notifications = %d, want 1", resolves)
	}
}
