package alerting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Resolve delivery is tracked per (finding, channel) (alerting-04): a
// channel that failed to resolve is retried on its own, a bounded number
// of times, and every failure leaves a 'resolve_error' row.
//
// Invalid input: not applicable; every input is a sage.findings or
// sage.alert_log row written by pg_sage itself.

// resolveChannel is a named fake channel. It fails the first failResolves
// resolve sends of each finding (every one when failResolves < 0) and
// counts resolves per finding, so rows left by other tests never count.
// Triggers always succeed.
type resolveChannel struct {
	name         string
	failResolves int
	mu           sync.Mutex
	resolves     map[int64]int
}

func (c *resolveChannel) Name() string { return c.name }

func (c *resolveChannel) Send(_ context.Context, a Alert) error {
	if !a.Resolved || len(a.Findings) != 1 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolves == nil {
		c.resolves = map[int64]int{}
	}
	id := a.Findings[0].ID
	c.resolves[id]++
	if c.failResolves < 0 || c.resolves[id] <= c.failResolves {
		return errors.New(c.name + ": upstream returned HTTP 503")
	}
	return nil
}

func (c *resolveChannel) resolveAttempts(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolves[id]
}

// alertTestPool connects to the test database and closes the pool after
// every cleanup of the test (insertAlertFinding deletes its rows), so no
// half-resolved finding leaks into a later test.
func alertTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := connectAlertTestDB(t)
	t.Cleanup(pool.Close)
	return pool
}

// logRecorder captures every log line of a Manager.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(level, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+" "+fmt.Sprintf(format, args...))
}

func (r *logRecorder) has(level, fragment string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if strings.HasPrefix(l, level+" ") && strings.Contains(l, fragment) {
			return true
		}
	}
	return false
}

// resolveFixture is one finding alerted on its channels, then resolved.
type resolveFixture struct {
	pool    *pgxpool.Pool
	manager *Manager
	finding int64
}

// newSentFixture alerts one critical finding on channels (one evaluate),
// so every channel holds a 'sent' row, and then resolves the finding.
func newSentFixture(
	t *testing.T, pool *pgxpool.Pool, logFn func(string, string, ...any),
	channels ...Channel,
) *resolveFixture {
	t.Helper()
	start := dbNow(t, pool)
	id := insertAlertFinding(t, pool, "critical", "alerting04")
	m := New(pool, ManagerConfig{}, map[string][]Channel{"critical": channels}, logFn)
	m.lastCheck = start.Add(-time.Second)
	fx := &resolveFixture{pool: pool, manager: m, finding: id}
	fx.evaluate(t, 1)
	for _, ch := range channels {
		if got := channelStatuses(t, pool, id, ch.Name()); len(got) != 1 || got[0] != "sent" {
			t.Fatalf("%s rows after trigger = %v, want [sent]", ch.Name(), got)
		}
	}
	markResolved(t, pool, id, "now()")
	return fx
}

func markResolved(t *testing.T, pool *pgxpool.Pool, id int64, at string) {
	t.Helper()
	// at is a fixed SQL expression chosen by the test, never input.
	if _, err := pool.Exec(context.Background(), `UPDATE sage.findings
		SET status = 'resolved', resolved_at = `+at+` WHERE id = $1`, id); err != nil {
		t.Fatalf("resolve finding %d: %v", id, err)
	}
}

func (fx *resolveFixture) evaluate(t *testing.T, cycles int) {
	t.Helper()
	for i := 0; i < cycles; i++ {
		if err := fx.manager.evaluate(context.Background()); err != nil {
			t.Fatalf("evaluate cycle %d: %v", i, err)
		}
	}
}

// channelStatuses lists the alert_log statuses of one finding on one
// channel, oldest first.
func channelStatuses(t *testing.T, pool *pgxpool.Pool, id int64, channel string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT status FROM sage.alert_log
		WHERE finding_id = $1 AND channel = $2 ORDER BY id`, id, channel)
	if err != nil {
		t.Fatalf("query alert_log of %s: %v", channel, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan alert_log of %s: %v", channel, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate alert_log of %s: %v", channel, err)
	}
	return out
}

func wantStatuses(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s alert_log statuses = %v, want %v", label, got, want)
	}
}

// TestResolve_FailedChannelRetriedAlone is the alerting-04 regression
// (CHECK-12, CHECK-13): Slack resolves, PagerDuty returns 503. PagerDuty
// is retried on the next cycle and Slack is not resolved twice.
func TestResolve_FailedChannelRetriedAlone(t *testing.T) {
	pool := alertTestPool(t)
	slack := &resolveChannel{name: "slack"}
	pd := &resolveChannel{name: "pagerduty", failResolves: 1}
	logs := &logRecorder{}
	fx := newSentFixture(t, pool, logs.log, slack, pd)

	fx.evaluate(t, 1)
	wantStatuses(t, "pagerduty after failure",
		channelStatuses(t, pool, fx.finding, "pagerduty"), "sent", "resolve_error")
	if !logs.has("ERROR", "pagerduty") {
		t.Errorf("failed resolve not logged at ERROR naming the channel: %v", logs.lines)
	}
	var msg string
	if err := pool.QueryRow(context.Background(), `SELECT error_message
		FROM sage.alert_log WHERE finding_id = $1 AND status = 'resolve_error'`,
		fx.finding).Scan(&msg); err != nil {
		t.Fatalf("CHECK-13: read resolve_error row: %v", err)
	}
	if !strings.Contains(msg, "HTTP 503") {
		t.Errorf("resolve_error message = %q, want the channel error text", msg)
	}

	fx.evaluate(t, 3)
	if got := pd.resolveAttempts(fx.finding); got != 2 {
		t.Errorf("pagerduty resolve attempts = %d, want 2 (503, then one success)", got)
	}
	if got := slack.resolveAttempts(fx.finding); got != 1 {
		t.Errorf("slack resolve attempts = %d, want exactly 1", got)
	}
	wantStatuses(t, "slack", channelStatuses(t, pool, fx.finding, "slack"),
		"sent", "resolved")
	wantStatuses(t, "pagerduty", channelStatuses(t, pool, fx.finding, "pagerduty"),
		"sent", "resolve_error", "resolved")
}

// TestResolve_AttemptCapIsFivePerChannel: with 4 resolve_error rows a
// channel is still retried; after the 5th it stops for good, while a
// healthy sibling channel resolves once.
func TestResolve_AttemptCapIsFivePerChannel(t *testing.T) {
	pool := alertTestPool(t)
	slack := &resolveChannel{name: "slack"}
	pd := &resolveChannel{name: "pagerduty", failResolves: -1}
	logs := &logRecorder{}
	fx := newSentFixture(t, pool, logs.log, slack, pd)

	fx.evaluate(t, 4)
	if got := pd.resolveAttempts(fx.finding); got != 4 {
		t.Fatalf("pagerduty attempts after 4 cycles = %d, want 4", got)
	}
	if !logs.has("ERROR", "attempt 4 of 5, retrying next cycle") {
		t.Errorf("4th failure not logged as a retry: %v", logs.lines)
	}
	fx.evaluate(t, 1)
	if got := pd.resolveAttempts(fx.finding); got != 5 {
		t.Fatalf("channel with 4 resolve_error rows not retried: attempts = %d", got)
	}
	if !logs.has("ERROR", "attempt 5 of 5, giving up; close the alert in pagerduty") {
		t.Errorf("5th failure not logged as giving up: %v", logs.lines)
	}
	fx.evaluate(t, 3)
	if got := pd.resolveAttempts(fx.finding); got != 5 {
		t.Errorf("channel retried past the cap: attempts = %d, want 5", got)
	}
	wantStatuses(t, "pagerduty", channelStatuses(t, pool, fx.finding, "pagerduty"),
		"sent", "resolve_error", "resolve_error", "resolve_error",
		"resolve_error", "resolve_error")
	if got := slack.resolveAttempts(fx.finding); got != 1 {
		t.Errorf("slack resolve attempts = %d, want 1", got)
	}
}

// TestResolve_OnlyRoutedChannelsThatWereSent: a channel removed from the
// routes is skipped (no send, no row), and a channel added after the
// trigger never sent this finding, so it gets no resolve either.
func TestResolve_OnlyRoutedChannelsThatWereSent(t *testing.T) {
	pool := alertTestPool(t)
	slack := &resolveChannel{name: "slack"}
	pd := &resolveChannel{name: "pagerduty"}
	fx := newSentFixture(t, pool, noop, slack, pd)
	added := &resolveChannel{name: "webhook-ops"}
	fx.manager.routes = map[string][]Channel{"critical": {slack, added}}

	fx.evaluate(t, 2)
	if got := pd.resolveAttempts(fx.finding); got != 0 {
		t.Errorf("unrouted pagerduty got %d resolves, want 0", got)
	}
	wantStatuses(t, "pagerduty", channelStatuses(t, pool, fx.finding, "pagerduty"), "sent")
	if got := added.resolveAttempts(fx.finding); got != 0 {
		t.Errorf("channel that never sent the finding got %d resolves", got)
	}
	wantStatuses(t, "webhook-ops", channelStatuses(t, pool, fx.finding, "webhook-ops"))
	if got := slack.resolveAttempts(fx.finding); got != 1 {
		t.Errorf("slack resolve attempts = %d, want 1", got)
	}
}

// TestResolve_NoRouteForSeverityLeavesNoRow: when the finding's severity
// has no route at all, nothing is sent and nothing is written.
func TestResolve_NoRouteForSeverityLeavesNoRow(t *testing.T) {
	pool := alertTestPool(t)
	slack := &resolveChannel{name: "slack"}
	fx := newSentFixture(t, pool, noop, slack)
	fx.manager.routes = map[string][]Channel{"warning": {slack}}

	fx.evaluate(t, 2)
	if got := slack.resolveAttempts(fx.finding); got != 0 {
		t.Errorf("resolve sent without a critical route: %d", got)
	}
	wantStatuses(t, "slack", channelStatuses(t, pool, fx.finding, "slack"), "sent")
}

// TestResolve_NeverSentFindingGetsNothing: a finding resolved before any
// channel delivered it (no row, or only an 'error' row) gets no resolve.
func TestResolve_NeverSentFindingGetsNothing(t *testing.T) {
	pool := alertTestPool(t)
	silent := insertAlertFinding(t, pool, "critical", "alerting04_unsent")
	failed := insertAlertFinding(t, pool, "critical", "alerting04_failed")
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.alert_log
		(finding_id, severity, channel, dedup_key, status, error_message)
		VALUES ($1, 'critical', 'slack', 'k', 'error', 'HTTP 500')`, failed); err != nil {
		t.Fatalf("seed error row: %v", err)
	}
	markResolved(t, pool, silent, "now()")
	markResolved(t, pool, failed, "now()")
	slack := &resolveChannel{name: "slack"}
	m := New(pool, ManagerConfig{}, map[string][]Channel{"critical": {slack}}, noop)
	fx := &resolveFixture{pool: pool, manager: m}
	fx.evaluate(t, 2)

	if got := slack.resolveAttempts(silent) + slack.resolveAttempts(failed); got != 0 {
		t.Errorf("resolve sent for a never-delivered finding: %d", got)
	}
	wantStatuses(t, "unsent", channelStatuses(t, pool, silent, "slack"))
	wantStatuses(t, "failed", channelStatuses(t, pool, failed, "slack"), "error")
}

// TestResolve_WindowEdge: a finding resolved 23 hours ago is still
// resolved on its channel; one resolved 25 hours ago is past the window.
func TestResolve_WindowEdge(t *testing.T) {
	pool := alertTestPool(t)
	slack := &resolveChannel{name: "slack"}
	inside := newSentFixture(t, pool, noop, slack)
	markResolved(t, pool, inside.finding, "now() - interval '23 hours'")
	inside.evaluate(t, 1)
	wantStatuses(t, "inside window", channelStatuses(t, pool, inside.finding, "slack"),
		"sent", "resolved")

	late := &resolveChannel{name: "slack"}
	outside := newSentFixture(t, pool, noop, late)
	markResolved(t, pool, outside.finding, "now() - interval '25 hours'")
	outside.evaluate(t, 1)
	if got := late.resolveAttempts(outside.finding); got != 0 {
		t.Errorf("resolve sent for a finding resolved 25 h ago: %d", got)
	}
	wantStatuses(t, "outside window", channelStatuses(t, pool, outside.finding, "slack"),
		"sent")
}

// TestResolve_FleetManagersKeepSeparateRows: two Managers on two
// databases run their resolve cycles at the same time. A PagerDuty failure
// on one database is retried there only; the other's rows are untouched.
func TestResolve_FleetManagersKeepSeparateRows(t *testing.T) {
	ordersPool := alertTestPool(t)
	billingPool := connectExtraAlertDB(t, "alerting04_billing")
	ordersPD := &resolveChannel{name: "pagerduty", failResolves: 1}
	billingPD := &resolveChannel{name: "pagerduty"}
	orders := newSentFixture(t, ordersPool, noop, ordersPD)
	billing := newSentFixture(t, billingPool, noop, billingPD)

	for cycle := 0; cycle < 3; cycle++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, fx := range []*resolveFixture{orders, billing} {
			wg.Add(1)
			go func(i int, m *Manager) {
				defer wg.Done()
				errs[i] = m.evaluate(context.Background())
			}(i, fx.manager)
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	wantStatuses(t, "orders", channelStatuses(t, ordersPool, orders.finding, "pagerduty"),
		"sent", "resolve_error", "resolved")
	wantStatuses(t, "billing", channelStatuses(t, billingPool, billing.finding, "pagerduty"),
		"sent", "resolved")
	a := ordersPD.resolveAttempts(orders.finding)
	if b := billingPD.resolveAttempts(billing.finding); a != 2 || b != 1 {
		t.Errorf("resolve attempts orders=%d billing=%d, want 2 and 1", a, b)
	}
}

// alertLogSeverities lists the severities of the alert_log rows with the
// given status for the given findings, in id (insert) order.
func alertLogSeverities(t *testing.T, pool *pgxpool.Pool, status string, ids []int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT severity FROM sage.alert_log
		WHERE finding_id = ANY($1) AND status = $2 ORDER BY id`, ids, status)
	if err != nil {
		t.Fatalf("query alert_log severities: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan alert_log severity: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate alert_log severities: %v", err)
	}
	return out
}

// TestEvaluate_CriticalLoggedFirst (alerting-05): one critical and three
// warnings in sage.findings leave alert_log ids in critical-first order.
// Five rounds, each with fresh findings, so a random group order cannot
// pass by luck (1 in 32).
func TestEvaluate_CriticalLoggedFirst(t *testing.T) {
	pool := alertTestPool(t)
	for round := 0; round < 5; round++ {
		start := dbNow(t, pool)
		var ids []int64
		for i := 0; i < 3; i++ {
			ids = append(ids, insertAlertFinding(t, pool, "warning", "alerting05_w"))
		}
		ids = append(ids, insertAlertFinding(t, pool, "critical", "alerting05_c"))
		ch := &orderChannel{name: "slack"}
		m := New(pool, ManagerConfig{}, routeAll(ch, "critical", "warning"), noop)
		m.lastCheck = start
		if err := m.evaluate(context.Background()); err != nil {
			t.Fatalf("round %d: evaluate: %v", round, err)
		}
		got := strings.Join(alertLogSeverities(t, pool, "sent", ids), ",")
		if want := "critical,warning,warning,warning"; got != want {
			t.Fatalf("round %d: alert_log order = %s, want %s", round, got, want)
		}
	}
}

// TestEvaluate_CriticalResolvedFirst (alerting-05): resolve notifications
// walk critical first too, even when the warning has the lower id.
func TestEvaluate_CriticalResolvedFirst(t *testing.T) {
	pool := alertTestPool(t)
	start := dbNow(t, pool)
	warn := insertAlertFinding(t, pool, "warning", "alerting05_rw")
	crit := insertAlertFinding(t, pool, "critical", "alerting05_rc")
	ch := &orderChannel{name: "slack"}
	m := New(pool, ManagerConfig{}, routeAll(ch, "critical", "warning"), noop)
	m.lastCheck = start.Add(-time.Second)
	fx := &resolveFixture{pool: pool, manager: m}
	fx.evaluate(t, 1)
	markResolved(t, pool, warn, "now()")
	markResolved(t, pool, crit, "now()")
	fx.evaluate(t, 1)

	ids := []int64{warn, crit}
	got := strings.Join(alertLogSeverities(t, pool, "resolved", ids), ",")
	if got != "critical,warning" {
		t.Errorf("resolved rows = %s, want critical,warning", got)
	}
	var resolvedSends []int64
	for _, s := range ch.sent {
		if s.resolved {
			resolvedSends = append(resolvedSends, s.id)
		}
	}
	if fmt.Sprint(resolvedSends) != fmt.Sprint([]int64{crit, warn}) {
		t.Errorf("resolve sends = %v, want [%d %d]", resolvedSends, crit, warn)
	}
}
