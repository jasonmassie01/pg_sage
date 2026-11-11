package alerting

import (
	"context"
	"fmt"
	"time"
)

// maxPendingErrorRows bounds retries of a finding whose delivery keeps
// failing (rows are per channel per attempt) within pendingWindow.
// maxResolveAttempts bounds the failed resolve notifications ('resolve_error'
// rows) of one finding on one channel.
const (
	maxPendingErrorRows = 10
	maxResolveAttempts  = 5
	pendingWindow       = "24 hours"
)

// pendingQuery selects open findings whose most recent alert_log row is
// a failed ('error') or quiet-hours-deferred ('deferred') attempt, i.e.
// no channel has delivered them since (SURF-15, G7-B19).
const pendingQuery = `
SELECT ` + findingColumns + `
FROM sage.findings f
JOIN LATERAL (
    SELECT a.status, a.sent_at
    FROM sage.alert_log a
    WHERE a.finding_id = f.id
    ORDER BY a.sent_at DESC, a.id DESC
    LIMIT 1
) last ON true
WHERE f.status = 'open'
  AND last.status IN ('error', 'deferred')
  AND last.sent_at > now() - interval '` + pendingWindow + `'
  AND (SELECT count(*) FROM sage.alert_log e
        WHERE e.finding_id = f.id AND e.status = 'error'
          AND e.sent_at > now() - interval '` + pendingWindow + `') < $1`

// insertDeferred records one 'deferred' row per quiet-hours episode.
const insertDeferred = `
INSERT INTO sage.alert_log
    (finding_id, severity, channel, dedup_key, status, error_message)
SELECT $1, $2, 'quiet_hours', $3, 'deferred', 'deferred by quiet hours'
WHERE COALESCE((SELECT a.status FROM sage.alert_log a
                 WHERE a.finding_id = $1
                 ORDER BY a.sent_at DESC, a.id DESC LIMIT 1), '')
      <> 'deferred'`

// resolvePendingQuery returns one row per (finding, channel) still owed a
// resolve notification (alerting-04): the finding resolved within
// pendingWindow, the channel delivered it ('sent'), has not resolved it
// and has fewer than $1 failed resolves, whose count is returned too.
const resolvePendingQuery = `
SELECT ` + findingColumns + `, s.channel, e.failures
FROM sage.findings f
JOIN LATERAL (
    SELECT DISTINCT a.channel
    FROM sage.alert_log a
    WHERE a.finding_id = f.id AND a.status = 'sent'
) s ON true
JOIN LATERAL (
    SELECT count(*) AS failures
    FROM sage.alert_log x
    WHERE x.finding_id = f.id AND x.channel = s.channel
      AND x.status = 'resolve_error'
) e ON true
WHERE f.status = 'resolved'
  AND f.resolved_at > now() - interval '` + pendingWindow + `'
  AND e.failures < $1
  AND NOT EXISTS (SELECT 1 FROM sage.alert_log r
                   WHERE r.finding_id = f.id AND r.channel = s.channel
                     AND r.status = 'resolved')
ORDER BY f.id, s.channel`

// resolveRow is one (finding, channel) row of resolvePendingQuery.
type resolveRow struct {
	finding  AlertFinding
	channel  string
	failures int
}

// resolveTarget is a resolved finding with the channels still owed its
// resolve notification and their failed attempts so far.
type resolveTarget struct {
	finding  AlertFinding
	channels map[string]bool
	failures map[string]int
}

// groupResolveRows folds rows into one target per finding, in row order.
func groupResolveRows(rows []resolveRow) []resolveTarget {
	var out []resolveTarget
	index := map[int64]int{}
	for _, r := range rows {
		i, ok := index[r.finding.ID]
		if !ok {
			i = len(out)
			index[r.finding.ID] = i
			out = append(out, resolveTarget{finding: r.finding,
				channels: map[string]bool{}, failures: map[string]int{}})
		}
		out[i].channels[r.channel] = true
		out[i].failures[r.channel] = r.failures
	}
	return out
}

// seedQuery restores deliveries into the throttle after a restart so
// every open finding is not re-alerted (G7-B32): the last delivery of
// each key whose finding is still open, however old, or that was sent
// within $1 minutes (the longest repeat interval plus an hour), with the
// first delivery of that finding since its last resolve (alerting-07).
// A resolve logged after the last delivery ends the episode (-1), so a
// row reopened with the same id is alerted as new.
const seedQuery = `
SELECT DISTINCT ON (a.dedup_key) a.dedup_key, a.severity,
       CASE WHEN r.ended_at IS NOT NULL THEN -1
            ELSE COALESCE(a.finding_id, 0) END,
       a.sent_at,
       COALESCE((SELECT min(b.sent_at) FROM sage.alert_log b
                  WHERE b.finding_id = a.finding_id AND b.status = 'sent'
                    AND b.sent_at > COALESCE(r.began_after, '-infinity')),
                a.sent_at)
FROM sage.alert_log a
CROSS JOIN LATERAL (
    SELECT max(x.sent_at) FILTER (WHERE x.sent_at > a.sent_at) AS ended_at,
           max(x.sent_at) FILTER (WHERE x.sent_at <= a.sent_at) AS began_after
    FROM sage.alert_log x
    WHERE x.finding_id = a.finding_id
      AND x.status IN ('resolved', 'resolve_error')
) r
WHERE a.status = 'sent'
  AND (a.sent_at > now() - make_interval(mins => $1)
       OR EXISTS (SELECT 1 FROM sage.findings f
                   WHERE f.id = a.finding_id AND f.status = 'open'))
ORDER BY a.dedup_key, a.sent_at DESC, a.id DESC`

func (m *Manager) queryPending(ctx context.Context) ([]AlertFinding, error) {
	return m.scanFindings(ctx, pendingQuery, maxPendingErrorRows)
}

func (m *Manager) recordDeferred(
	ctx context.Context, f AlertFinding, sev, key string,
) {
	if _, err := m.pool.Exec(ctx, insertDeferred, f.ID, sev, key); err != nil {
		m.logFn("ERROR", "record deferred alert: %v", err)
	}
}

func (m *Manager) queryResolvePending(ctx context.Context) ([]resolveTarget, error) {
	rows, err := m.pool.Query(ctx, resolvePendingQuery, maxResolveAttempts)
	if err != nil {
		return nil, fmt.Errorf("execute resolve pending query: %w", err)
	}
	defer rows.Close()
	var out []resolveRow
	for rows.Next() {
		var r resolveRow
		var failures int64
		if r.finding, err = scanFindingRow(rows, &r.channel, &failures); err != nil {
			return nil, err
		}
		r.failures = int(failures)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate resolve pending rows: %w", err)
	}
	targets := groupResolveRows(out)
	orderResolveTargets(targets)
	return targets, nil
}

// sendResolutions sends each resolved, previously alerted finding's
// resolve notification to every channel that delivered it, is still
// routed for its severity and has not resolved it yet. Each channel is
// tracked on its own: success writes 'resolved', failure 'resolve_error'
// and is retried next cycle, at most maxResolveAttempts times.
// Findings are walked critical first, like triggers (alerting-05).
func (m *Manager) sendResolutions(ctx context.Context) {
	targets, err := m.queryResolvePending(ctx)
	if err != nil {
		m.logFn("ERROR", "query pending resolve notifications: %v", err)
		return
	}
	for _, target := range targets {
		m.resolveFinding(ctx, target)
	}
}

func (m *Manager) resolveFinding(ctx context.Context, target resolveTarget) {
	f := target.finding
	alert := m.newAlert(f.Severity, f)
	alert.Resolved = true
	key := FormatDedupKey(f.Category, f.ObjectIdentifier)
	m.throttle.endEpisode(key, f.ID)
	for _, ch := range m.routes[f.Severity] {
		name := ch.Name()
		if !target.channels[name] {
			continue
		}
		if err := ch.Send(ctx, alert); err != nil {
			m.logResolveFailure(f.ID, name, target.failures[name]+1, err)
			m.logAlert(ctx, f.ID, f.Severity, name, key, "resolve_error", err.Error())
			continue
		}
		m.logAlert(ctx, f.ID, f.Severity, name, key, "resolved", "")
	}
}

func (m *Manager) logResolveFailure(id int64, channel string, attempt int, err error) {
	if attempt >= maxResolveAttempts {
		m.logFn("ERROR", "resolve of finding %d to %s failed (attempt %d of %d, "+
			"giving up; close the alert in %s by hand): %v",
			id, channel, attempt, maxResolveAttempts, channel, err)
		return
	}
	m.logFn("ERROR", "resolve of finding %d to %s failed (attempt %d of %d, "+
		"retrying next cycle): %v", id, channel, attempt, maxResolveAttempts, err)
}

// seedThrottle loads recent deliveries once per Manager.
func (m *Manager) seedThrottle(ctx context.Context) {
	m.mu.Lock()
	done := m.seeded
	m.seeded = true
	m.mu.Unlock()
	if done {
		return
	}
	window := int(m.throttle.SeedWindow() / time.Minute)
	rows, err := m.pool.Query(ctx, seedQuery, window)
	if err != nil {
		m.logFn("WARN", "alerting: seed throttle from alert_log: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key, sev string
		var episode int64
		var at, first time.Time
		if err := rows.Scan(&key, &sev, &episode, &at, &first); err != nil {
			m.logFn("WARN", "alerting: scan alert_log seed: %v", err)
			return
		}
		m.throttle.restore(key, sev, episode, first, at)
	}
	if err := rows.Err(); err != nil {
		m.logFn("WARN", "alerting: iterate alert_log seed: %v", err)
	}
}
