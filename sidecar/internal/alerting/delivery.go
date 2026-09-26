package alerting

import (
	"context"
	"time"
)

// maxPendingErrorRows bounds retries of a finding whose delivery keeps
// failing (rows are per channel per attempt) within pendingWindow.
const (
	maxPendingErrorRows = 10
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

// resolvedQuery selects recently resolved findings that were alerted
// and have not had a resolve notification delivered yet (G7-B14).
const resolvedQuery = `
SELECT ` + findingColumns + `
FROM sage.findings f
WHERE f.status = 'resolved'
  AND f.resolved_at > now() - interval '` + pendingWindow + `'
  AND EXISTS (SELECT 1 FROM sage.alert_log a
               WHERE a.finding_id = f.id AND a.status = 'sent')
  AND NOT EXISTS (SELECT 1 FROM sage.alert_log a
                   WHERE a.finding_id = f.id AND a.status = 'resolved')`

// seedQuery restores recent deliveries into the throttle after a
// restart so every open finding is not re-alerted (G7-B32).
const seedQuery = `
SELECT DISTINCT ON (dedup_key) dedup_key, severity, sent_at
FROM sage.alert_log
WHERE status = 'sent' AND sent_at > now() - interval '6 hours'
ORDER BY dedup_key, sent_at DESC`

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

// sendResolutions notifies the channels of each resolved, previously
// alerted finding once; failures are retried on later cycles.
func (m *Manager) sendResolutions(ctx context.Context) {
	resolved, err := m.scanFindings(ctx, resolvedQuery)
	if err != nil {
		m.logFn("ERROR", "query resolved findings: %v", err)
		return
	}
	for _, f := range resolved {
		channels, ok := m.routes[f.Severity]
		if !ok {
			continue
		}
		alert := m.newAlert(f.Severity, f)
		alert.Resolved = true
		key := FormatDedupKey(f.Category, f.ObjectIdentifier)
		for _, ch := range channels {
			if err := ch.Send(ctx, alert); err != nil {
				m.logFn("ERROR", "resolve to %s failed: %v", ch.Name(), err)
				continue
			}
			m.logAlert(ctx, f.ID, f.Severity, ch.Name(), key, "resolved", "")
		}
	}
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
	rows, err := m.pool.Query(ctx, seedQuery)
	if err != nil {
		m.logFn("WARN", "alerting: seed throttle from alert_log: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key, sev string
		var at time.Time
		if err := rows.Scan(&key, &sev, &at); err != nil {
			m.logFn("WARN", "alerting: scan alert_log seed: %v", err)
			return
		}
		m.throttle.RecordAt(key, sev, at)
	}
	if err := rows.Err(); err != nil {
		m.logFn("WARN", "alerting: iterate alert_log seed: %v", err)
	}
}
