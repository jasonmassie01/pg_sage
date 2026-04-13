package rca

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// ---------------------------------------------------------------------------
// Incident builder
// ---------------------------------------------------------------------------

func buildIncident(
	at time.Time,
	severity string,
	signalIDs []string,
	rootCause string,
	chain []ChainLink,
	affected []string,
	sql string,
	risk string,
) Incident {
	return Incident{
		DetectedAt:      at,
		Severity:        severity,
		RootCause:       rootCause,
		CausalChain:     chain,
		AffectedObjects: affected,
		SignalIDs:       signalIDs,
		RecommendedSQL:  sql,
		ActionRisk:      risk,
		Source:          "deterministic",
		Confidence:      0.85,
	}
}

// newUUID generates a random UUID v4 string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 2
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---------------------------------------------------------------------------
// Snapshot helpers
// ---------------------------------------------------------------------------

func totalWALBytes(snap *collector.Snapshot) int64 {
	var total int64
	for _, q := range snap.Queries {
		total += q.WALBytes
	}
	return total
}

func queryReadsMap(snap *collector.Snapshot) map[int64]int64 {
	m := make(map[int64]int64, len(snap.Queries))
	for _, q := range snap.Queries {
		m[q.QueryID] = q.SharedBlksRead
	}
	return m
}

func blockedTables(sig *Signal) []string {
	if tables, ok := sig.Metrics["blocked_tables"].([]string); ok {
		return tables
	}
	return nil
}

// ---------------------------------------------------------------------------
// Severity helpers
// ---------------------------------------------------------------------------

func severityForPct(pct, critThreshold int) string {
	if pct >= critThreshold {
		return "critical"
	}
	return "warning"
}

func severityForFloat(val, critThreshold float64) string {
	if val >= critThreshold {
		return "critical"
	}
	return "warning"
}

func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Metric extraction from Signal
// ---------------------------------------------------------------------------

func intMetric(s *Signal, key string) int {
	switch v := s.Metrics[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func floatMetric(s *Signal, key string) float64 {
	switch v := s.Metrics[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func stringMetric(s *Signal, key string) string {
	if v, ok := s.Metrics[key].(string); ok {
		return v
	}
	return ""
}

func boolMetric(s *Signal, key string) bool {
	if v, ok := s.Metrics[key].(bool); ok {
		return v
	}
	return false
}

// buildLogIncident is like buildIncident but sets Source to
// "log_deterministic" for log-based signals.
func buildLogIncident(
	at time.Time,
	severity string,
	signalIDs []string,
	rootCause string,
	chain []ChainLink,
	affected []string,
	sql string,
	risk string,
) Incident {
	inc := buildIncident(at, severity, signalIDs, rootCause,
		chain, affected, sql, risk)
	inc.Source = "log_deterministic"
	return inc
}

// ---------------------------------------------------------------------------
// String / sort helpers
// ---------------------------------------------------------------------------

func sortedCopy(ss []string) []string {
	cp := make([]string, len(ss))
	copy(cp, ss)
	sort.Strings(cp)
	return cp
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// parseIntervalSeconds extracts seconds from a PostgreSQL interval string
// like "00:00:05.123" or returns 0 if nil/unparseable.
func parseIntervalSeconds(s *string) float64 {
	if s == nil || *s == "" {
		return 0
	}
	raw := *s
	// PostgreSQL interval format: HH:MM:SS.microseconds
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return 0
	}
	var h, m int
	var sec float64
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0
	}
	if _, err := fmt.Sscanf(parts[2], "%f", &sec); err != nil {
		return 0
	}
	return float64(h*3600+m*60) + sec
}

// marshalChain produces a JSON array string for the causal chain,
// suitable for passing to pgx as a jsonb parameter.
func marshalChain(chain []ChainLink) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, link := range chain {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b,
			`{"order":%d,"signal":%q,"description":%q,"evidence":%q}`,
			link.Order, link.Signal, link.Description, link.Evidence)
	}
	b.WriteByte(']')
	return b.String()
}
