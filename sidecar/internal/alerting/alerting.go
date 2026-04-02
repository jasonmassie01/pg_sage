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
	findingsQuery        = `
SELECT id, category, severity, title,
       COALESCE(object_type, ''),
       COALESCE(object_identifier, ''),
       occurrence_count,
       COALESCE(recommendation, ''),
       created_at, last_seen
FROM sage.findings
WHERE status = 'open' AND last_seen > $1
ORDER BY
    CASE severity
        WHEN 'critical' THEN 0
        WHEN 'warning'  THEN 1
        ELSE 2
    END,
    last_seen DESC`

	insertAlertLog = `
INSERT INTO sage.alert_log
    (finding_id, severity, channel, dedup_key,
     status, error_message)
VALUES ($1, $2, $3, $4, $5, $6)`
)

// Manager manages alert routing, deduplication, and dispatch.
type Manager struct {
	pool      *pgxpool.Pool
	mcfg      ManagerConfig
	routes    map[string][]Channel
	throttle  *Throttle
	lastCheck time.Time
	logFn     func(string, string, ...any)
	mu        sync.Mutex
}

// New creates an alert Manager.
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

// evaluate queries new findings and dispatches alerts.
func (m *Manager) evaluate(ctx context.Context) error {
	if m.pool == nil {
		return fmt.Errorf("query findings: pool is nil")
	}

	m.mu.Lock()
	since := m.lastCheck
	m.mu.Unlock()

	findings, err := m.queryFindings(ctx, since)
	if err != nil {
		return fmt.Errorf("query findings: %w", err)
	}

	if len(findings) == 0 {
		m.mu.Lock()
		m.lastCheck = time.Now()
		m.mu.Unlock()
		return nil
	}

	grouped := groupBySeverity(findings)

	for sev, group := range grouped {
		channels, ok := m.routes[sev]
		if !ok {
			continue
		}
		m.dispatchGroup(ctx, sev, group, channels)
	}

	m.mu.Lock()
	m.lastCheck = time.Now()
	m.mu.Unlock()
	return nil
}

// queryFindings loads open findings updated since the given time.
func (m *Manager) queryFindings(
	ctx context.Context, since time.Time,
) ([]AlertFinding, error) {
	rows, err := m.pool.Query(ctx, findingsQuery, since)
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
		if !m.throttle.ShouldAlert(key, sev) {
			continue
		}

		alert := Alert{
			Findings:  []AlertFinding{f},
			Severity:  sev,
			Timestamp: time.Now(),
		}
		m.dispatch(ctx, channels, alert, f.ID, key)
		m.throttle.Record(key, sev)
	}
}

func (m *Manager) dispatch(
	ctx context.Context,
	channels []Channel,
	alert Alert,
	findingID int64,
	dedupKey string,
) {
	for _, ch := range channels {
		err := ch.Send(ctx, alert)
		status := "sent"
		errMsg := ""
		if err != nil {
			status = "error"
			errMsg = err.Error()
			m.logFn("ERROR", "alert to %s failed: %v",
				ch.Name(), err)
		} else {
			m.logFn("INFO", "alert sent to %s for %s",
				ch.Name(), dedupKey)
		}
		m.logAlert(
			ctx, findingID, alert.Severity,
			ch.Name(), dedupKey, status, errMsg,
		)
	}
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
