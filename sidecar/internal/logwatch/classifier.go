package logwatch

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

// signalPattern defines one pattern the classifier matches against log entries.
type signalPattern struct {
	ID         string
	Severity   string
	SQLStates  []string // exact match (or prefix for XX/28)
	Substrings []string // case-insensitive, ALL must match
	MinLevel   string   // minimum ErrorLevel
	OptIn      bool     // requires explicit config enable
}

// patterns is the full catalog of 18 signal definitions.
var patterns = []signalPattern{
	{ID: "log_deadlock_detected", Severity: "critical",
		SQLStates: []string{"40P01"}, MinLevel: "ERROR"},
	{ID: "log_connection_refused", Severity: "critical",
		SQLStates: []string{"53300"}, MinLevel: "FATAL"},
	{ID: "log_out_of_memory", Severity: "critical",
		SQLStates: []string{"53200"}, MinLevel: "ERROR"},
	{ID: "log_disk_full", Severity: "critical",
		SQLStates: []string{"53100"}, MinLevel: "ERROR"},
	{ID: "log_panic_server_crash", Severity: "critical",
		Substrings: []string{"server process was terminated by signal"},
		MinLevel: "LOG"},
	{ID: "log_data_corruption", Severity: "critical",
		MinLevel: "WARNING"},
	{ID: "log_txid_wraparound_warning", Severity: "critical",
		Substrings: []string{"must be vacuumed within"},
		MinLevel: "WARNING"},
	{ID: "log_archive_failed", Severity: "critical",
		Substrings: []string{"archive command failed"}, MinLevel: "LOG"},
	{ID: "log_temp_file_created", Severity: "warning",
		Substrings: []string{"temporary file"}, MinLevel: "LOG"},
	{ID: "log_checkpoint_too_frequent", Severity: "warning",
		Substrings: []string{"checkpoints are occurring too frequently"},
		MinLevel: "LOG"},
	{ID: "log_lock_timeout", Severity: "warning",
		SQLStates: []string{"55P03"}, MinLevel: "ERROR"},
	{ID: "log_statement_timeout", Severity: "warning",
		SQLStates: []string{"57014"}, MinLevel: "ERROR"},
	{ID: "log_replication_conflict", Severity: "warning",
		Substrings: []string{"conflict with recovery"},
		MinLevel: "ERROR"},
	{ID: "log_wal_segment_removed", Severity: "critical",
		Substrings: []string{"WAL segment has already been removed"},
		MinLevel: "ERROR"},
	{ID: "log_autovacuum_cancel", Severity: "warning",
		Substrings: []string{"canceling autovacuum task"},
		MinLevel: "LOG"},
	{ID: "log_replication_slot_inactive", Severity: "warning",
		Substrings: []string{"replication slot", "inactive"},
		MinLevel: "WARNING"},
	{ID: "log_authentication_failure", Severity: "warning",
		MinLevel: "FATAL"},
	{ID: "log_slow_query", Severity: "info",
		Substrings: []string{"duration:"}, MinLevel: "LOG", OptIn: true},
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// levelRank maps PostgreSQL severity to a numeric rank for comparison.
func levelRank(level string) int {
	switch strings.ToUpper(level) {
	case "LOG":
		return 1
	case "WARNING":
		return 2
	case "ERROR":
		return 3
	case "FATAL":
		return 4
	case "PANIC":
		return 5
	default:
		return 0
	}
}

var reTempFileSize = regexp.MustCompile(`size\s+(\d+)`)

// parseTempFileSize extracts the byte count from a PG "temporary file" message.
// Returns 0 if not found.
func parseTempFileSize(msg string) int64 {
	m := reTempFileSize.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

var reSlowDuration = regexp.MustCompile(
	`duration:\s+([\d.]+)\s+ms`,
)

// parseSlowQueryDuration extracts the millisecond value from a PG
// "duration: NNN.NNN ms" message. Returns 0 if not found.
func parseSlowQueryDuration(msg string) float64 {
	m := reSlowDuration.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return f
}

// ---------------------------------------------------------------------------
// Classifier
// ---------------------------------------------------------------------------

// ClassifierConfig holds tunables for the Classifier.
type ClassifierConfig struct {
	DedupWindowS     int      // seconds; default 60
	ExcludeApps      []string // application_name values to skip
	SlowQueryEnabled bool
	TempFileMinBytes int64 // skip temp-file signals below this
	MaxLinesPerCycle int   // 0 = unlimited
}

// dedupEntry tracks repeated occurrences of (signalID, database).
type dedupEntry struct {
	firstSeen time.Time
	count     int
	pids      map[int]bool
}

// Classifier matches LogEntry values against the signal pattern table and
// returns rca.Signal values with deduplication.
type Classifier struct {
	dedupWindow      time.Duration
	excludeApps      map[string]bool
	slowQueryEnabled bool
	tempFileMinBytes int64
	maxLinesPerCycle int
	logFn            func(string, string, ...any)

	dedupBuf       map[string]*dedupEntry
	linesThisCycle int
	mu             sync.Mutex
}

// NewClassifier creates a Classifier from the given config.
func NewClassifier(
	cfg ClassifierConfig,
	logFn func(string, string, ...any),
) *Classifier {
	window := time.Duration(cfg.DedupWindowS) * time.Second
	if window == 0 {
		window = 60 * time.Second
	}
	apps := make(map[string]bool, len(cfg.ExcludeApps))
	for _, a := range cfg.ExcludeApps {
		apps[a] = true
	}
	return &Classifier{
		dedupWindow:      window,
		excludeApps:      apps,
		slowQueryEnabled: cfg.SlowQueryEnabled,
		tempFileMinBytes: cfg.TempFileMinBytes,
		maxLinesPerCycle: cfg.MaxLinesPerCycle,
		logFn:            logFn,
		dedupBuf:         make(map[string]*dedupEntry),
	}
}

// Classify evaluates a single LogEntry and returns a Signal if it matches
// a known pattern, or nil if no match / dedup suppresses it.
func (c *Classifier) Classify(entry LogEntry) *rca.Signal {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.maxLinesPerCycle > 0 && c.linesThisCycle >= c.maxLinesPerCycle {
		return nil
	}
	c.linesThisCycle++

	selfInflicted := c.excludeApps[entry.Application] && entry.Application != ""
	if selfInflicted && entry.SQLState != "40P01" {
		return nil
	}

	pat, ok := c.matchPattern(entry)
	if !ok {
		return nil
	}

	sig := c.buildSignal(pat, entry, selfInflicted)
	if sig == nil {
		return nil
	}

	if c.isDuplicate(pat.ID, entry.Database, entry.PID, entry.Timestamp) {
		return nil
	}
	return sig
}

// ResetCycle resets the per-cycle line counter. Called by Drain().
func (c *Classifier) ResetCycle() {
	c.mu.Lock()
	c.linesThisCycle = 0
	c.mu.Unlock()
}

// CleanExpiredDedup removes dedup entries older than dedupWindow.
func (c *Classifier) CleanExpiredDedup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, de := range c.dedupBuf {
		if now.Sub(de.firstSeen) > c.dedupWindow {
			delete(c.dedupBuf, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Internal matching
// ---------------------------------------------------------------------------

// matchPattern finds the first pattern that matches entry.
func (c *Classifier) matchPattern(entry LogEntry) (signalPattern, bool) {
	entryRank := levelRank(entry.ErrorLevel)
	lower := strings.ToLower(entry.Message)

	for _, pat := range patterns {
		if entryRank < levelRank(pat.MinLevel) {
			continue
		}
		if pat.OptIn && !c.optInEnabled(pat.ID) {
			continue
		}
		if c.matchesSQLStateOrSubstring(pat, entry, lower) {
			return pat, true
		}
	}
	return signalPattern{}, false
}

// matchesSQLStateOrSubstring checks SQLState then substring rules.
func (c *Classifier) matchesSQLStateOrSubstring(
	pat signalPattern, entry LogEntry, lower string,
) bool {
	switch pat.ID {
	case "log_panic_server_crash":
		return entry.ErrorLevel == "PANIC" ||
			strings.Contains(lower, "server process was terminated by signal")
	case "log_data_corruption":
		return strings.HasPrefix(entry.SQLState, "XX")
	case "log_authentication_failure":
		return strings.HasPrefix(entry.SQLState, "28")
	}
	if len(pat.SQLStates) > 0 && entry.SQLState != "" {
		for _, s := range pat.SQLStates {
			if entry.SQLState == s {
				return true
			}
		}
	}
	if len(pat.Substrings) > 0 {
		return allSubstringsMatch(lower, pat.Substrings)
	}
	return false
}

// allSubstringsMatch returns true if lower contains every substring
// (already lowered).
func allSubstringsMatch(lower string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(lower, strings.ToLower(sub)) {
			return false
		}
	}
	return true
}

func (c *Classifier) optInEnabled(id string) bool {
	if id == "log_slow_query" {
		return c.slowQueryEnabled
	}
	return false
}

// ---------------------------------------------------------------------------
// Signal building with pattern-specific logic
// ---------------------------------------------------------------------------

// buildSignal constructs an rca.Signal from a matched pattern and entry.
// Returns nil if the signal should be suppressed (e.g. temp file too small).
func (c *Classifier) buildSignal(
	pat signalPattern, entry LogEntry, selfInflicted bool,
) *rca.Signal {
	metrics := map[string]any{
		"database":    entry.Database,
		"user":        entry.User,
		"pid":         entry.PID,
		"error_level": entry.ErrorLevel,
		"sql_state":   entry.SQLState,
		"message":     Truncate(entry.Message, MaxRawMessageLen),
		"application": entry.Application,
	}
	if selfInflicted {
		metrics["self_inflicted"] = true
	}
	if entry.Detail != "" {
		metrics["detail"] = Truncate(entry.Detail, MaxRawMessageLen)
	}

	switch pat.ID {
	case "log_temp_file_created":
		sz := parseTempFileSize(entry.Message)
		if sz < c.tempFileMinBytes {
			return nil
		}
		metrics["temp_file_bytes"] = sz
	case "log_slow_query":
		dur := parseSlowQueryDuration(entry.Message)
		metrics["duration_ms"] = dur
		if entry.Query != "" {
			metrics["query"] = Truncate(entry.Query, MaxQueryLen)
		}
	}

	return &rca.Signal{
		ID:       pat.ID,
		FiredAt:  entry.Timestamp,
		Severity: pat.Severity,
		Metrics:  metrics,
	}
}

// ---------------------------------------------------------------------------
// Dedup
// ---------------------------------------------------------------------------

// isDuplicate returns true if (signalID, database) was already seen within
// the dedup window. On first occurrence it records the entry and returns false.
func (c *Classifier) isDuplicate(
	signalID, database string, pid int, ts time.Time,
) bool {
	key := signalID + "\x00" + database
	de, exists := c.dedupBuf[key]
	if !exists {
		c.dedupBuf[key] = &dedupEntry{
			firstSeen: ts,
			count:     1,
			pids:      map[int]bool{pid: true},
		}
		return false
	}
	if ts.Sub(de.firstSeen) > c.dedupWindow {
		// Window expired — start a new entry.
		c.dedupBuf[key] = &dedupEntry{
			firstSeen: ts,
			count:     1,
			pids:      map[int]bool{pid: true},
		}
		return false
	}
	de.count++
	if len(de.pids) < MaxAffectedPIDs {
		de.pids[pid] = true
	}
	return true
}
