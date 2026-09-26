package alerting

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultCheckInterval = 60
	findingColumns       = `f.id, f.category, f.severity, f.title,
       COALESCE(f.object_type, ''),
       COALESCE(f.object_identifier, ''),
       f.occurrence_count,
       COALESCE(f.recommendation, ''),
       f.created_at, f.last_seen`
	findingsQuery = `
SELECT ` + findingColumns + `
FROM sage.findings f
WHERE f.status = 'open' AND f.last_seen > $1
ORDER BY
    CASE f.severity
        WHEN 'critical' THEN 0
        WHEN 'warning'  THEN 1
        ELSE 2
    END,
    f.last_seen DESC`

	insertAlertLog = `
INSERT INTO sage.alert_log
    (finding_id, severity, channel, dedup_key,
     status, error_message)
VALUES ($1, $2, $3, $4, $5, $6)`
)

// Manager manages alert routing, deduplication, and dispatch.
//
// Reliability contract (SURF-15): the read watermark is the database
// clock captured BEFORE the findings query, so rows that change during
// a slow dispatch are read next cycle. A finding counts as delivered
// when at least one channel accepted it; when every channel failed, or
// quiet hours deferred it, the attempt is recorded in sage.alert_log
// (status 'error' / 'deferred') and the finding is re-read from there
// on later cycles (bounded attempts within a 24h window).
type Manager struct {
	pool      *pgxpool.Pool
	mcfg      ManagerConfig
	routes    map[string][]Channel
	throttle  *Throttle
	lastCheck time.Time
	seeded    bool
	logFn     func(string, string, ...any)
	mu        sync.Mutex
}

// New creates an alert Manager. Configuration problems (invalid
// timezone, malformed quiet hours) are logged as warnings.
func New(
	pool *pgxpool.Pool,
	mcfg ManagerConfig,
	routes map[string][]Channel,
	logFn func(string, string, ...any),
) *Manager {
	throttle := NewThrottle(
		mcfg.CooldownMinutes,
		mcfg.QuietHoursStart,
		mcfg.QuietHoursEnd,
		mcfg.Timezone,
	)
	for _, w := range throttle.Warnings() {
		logFn("WARN", "alerting: %s", w)
	}
	return &Manager{
		pool:      pool,
		mcfg:      mcfg,
		routes:    routes,
		throttle:  throttle,
		lastCheck: time.Now(),
		logFn:     logFn,
	}
}

// Throttle returns the underlying throttle (for testing).
func (m *Manager) Throttle() *Throttle { return m.throttle }

// Run starts the alert evaluation loop.
func (m *Manager) Run(ctx context.Context) {
	interval := m.mcfg.CheckIntervalSeconds
	if interval <= 0 {
		interval = defaultCheckInterval
	}
	ticker := time.NewTicker(
		time.Duration(interval) * time.Second,
	)
	defer ticker.Stop()

	m.logFn("INFO", "alerting started, interval=%ds", interval)

	for {
		select {
		case <-ctx.Done():
			m.logFn("INFO", "alerting stopped")
			return
		case <-ticker.C:
			if err := m.evaluate(ctx); err != nil {
				m.logFn("ERROR", "alert evaluate: %v", err)
			}
		}
	}
}

// evaluate queries new and pending findings and dispatches alerts.
func (m *Manager) evaluate(ctx context.Context) error {
	if m.pool == nil {
		return fmt.Errorf("query findings: pool is nil")
	}
	m.seedThrottle(ctx)
	snapshot, err := m.dbNow(ctx)
	if err != nil {
		return fmt.Errorf("query findings: %w", err)
	}
	m.mu.Lock()
	since := m.lastCheck
	m.mu.Unlock()

	findings, err := m.queryFindings(ctx, since)
	if err != nil {
		return fmt.Errorf("query findings: %w", err)
	}
	pending, err := m.queryPending(ctx)
	if err != nil {
		return fmt.Errorf("query pending alerts: %w", err)
	}
	for sev, group := range groupBySeverity(mergeFindings(findings, pending)) {
		if channels, ok := m.routes[sev]; ok {
			m.dispatchGroup(ctx, sev, group, channels)
		}
	}
	m.sendResolutions(ctx)

	m.mu.Lock()
	m.lastCheck = snapshot
	m.mu.Unlock()
	return nil
}

// dbNow reads the watermark from the database clock, the same clock
// that stamps findings.last_seen (G7-B18: no host/DB skew).
func (m *Manager) dbNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := m.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	return now, nil
}

// queryFindings loads open findings updated since the given time.
func (m *Manager) queryFindings(
	ctx context.Context, since time.Time,
) ([]AlertFinding, error) {
	return m.scanFindings(ctx, findingsQuery, since)
}

func (m *Manager) scanFindings(
	ctx context.Context, sql string, args ...any,
) ([]AlertFinding, error) {
	rows, err := m.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("execute findings query: %w", err)
	}
	defer rows.Close()

	var results []AlertFinding
	for rows.Next() {
		var f AlertFinding
		if err := rows.Scan(
			&f.ID, &f.Category, &f.Severity, &f.Title,
			&f.ObjectType, &f.ObjectIdentifier,
			&f.OccurrenceCount, &f.Recommendation,
			&f.FirstSeen, &f.LastSeen,
		); err != nil {
			return nil, fmt.Errorf("scan finding row: %w", err)
		}
		results = append(results, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate findings rows: %w", err)
	}
	return results, nil
}

func (m *Manager) dispatchGroup(
	ctx context.Context,
	sev string,
	findings []AlertFinding,
	channels []Channel,
) {
	for _, f := range findings {
		key := FormatDedupKey(f.Category, f.ObjectIdentifier)
		switch m.throttle.Decide(key, sev) {
		case DecisionThrottled:
			continue
		case DecisionDefer:
			m.recordDeferred(ctx, f, sev, key)
			continue
		}
		alert := m.newAlert(sev, f)
		// Only record the throttle key when at least one channel
		// actually delivered. Recording on total failure would suppress
		// re-firing for the whole cooldown window (H4); the failure is
		// retried from alert_log instead.
		if m.dispatch(ctx, channels, alert, f.ID, key) {
			m.throttle.Record(key, sev)
		}
	}
}

func (m *Manager) newAlert(sev string, findings ...AlertFinding) Alert {
	return Alert{
		Findings:  findings,
		Severity:  sev,
		Timestamp: time.Now(),
		Database:  m.mcfg.DatabaseName,
	}
}

// dispatch sends the alert to every channel and returns true if at
// least one channel delivered successfully. Failed rows are logged
// before successful ones so the newest alert_log row of a finding is
// 'error' only when no channel delivered.
func (m *Manager) dispatch(
	ctx context.Context,
	channels []Channel,
	alert Alert,
	findingID int64,
	dedupKey string,
) bool {
	var failed, delivered []string
	errs := map[string]string{}
	for _, ch := range channels {
		if err := ch.Send(ctx, alert); err != nil {
			failed = append(failed, ch.Name())
			errs[ch.Name()] = err.Error()
			m.logFn("ERROR", "alert to %s failed: %v", ch.Name(), err)
			continue
		}
		delivered = append(delivered, ch.Name())
		m.logFn("INFO", "alert sent to %s for %s", ch.Name(), dedupKey)
	}
	for _, name := range failed {
		m.logAlert(ctx, findingID, alert.Severity, name, dedupKey,
			"error", errs[name])
	}
	for _, name := range delivered {
		m.logAlert(ctx, findingID, alert.Severity, name, dedupKey, "sent", "")
	}
	return len(delivered) > 0
}

func (m *Manager) logAlert(
	ctx context.Context,
	findingID int64,
	severity, channel, dedupKey, status, errMsg string,
) {
	if m.pool == nil {
		return
	}
	_, err := m.pool.Exec(
		ctx, insertAlertLog,
		findingID, severity, channel,
		dedupKey, status, errMsg,
	)
	if err != nil {
		m.logFn("ERROR", "insert alert_log: %v", err)
	}
}

func groupBySeverity(
	findings []AlertFinding,
) map[string][]AlertFinding {
	groups := make(map[string][]AlertFinding)
	for _, f := range findings {
		groups[f.Severity] = append(groups[f.Severity], f)
	}
	return groups
}

// mergeFindings appends pending findings not already in fresh.
func mergeFindings(fresh, pending []AlertFinding) []AlertFinding {
	seen := make(map[int64]bool, len(fresh))
	for _, f := range fresh {
		seen[f.ID] = true
	}
	for _, f := range pending {
		if !seen[f.ID] {
			fresh = append(fresh, f)
			seen[f.ID] = true
		}
	}
	return fresh
}
