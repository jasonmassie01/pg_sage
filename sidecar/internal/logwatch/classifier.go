package logwatch

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/signalid"
)

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

// DefaultExcludeApplications is used when no exclude list is configured.
// The sidecar tags every pool connection with application_name=pg_sage,
// so its own statement/lock timeouts are not treated as incidents.
var DefaultExcludeApplications = []string{"pg_sage"}

// ClassifierConfig holds tunables for the Classifier.
type ClassifierConfig struct {
	DedupWindowS     int      // seconds; default 60
	ExcludeApps      []string // application_name values to skip; empty = default
	SlowQueryEnabled bool
	TempFileMinBytes int64 // skip temp-file signals below this
	MaxLinesPerCycle int   // 0 = unlimited
	// DeadlockStatementText is rca.deadlock_statement_text: "redacted"
	// keeps each deadlock participant's redacted statement text; anything
	// else keeps only its fingerprint (rca-07b).
	DeadlockStatementText string
}

// dedupEntry tracks repeated occurrences of (signalID, database) within
// one dedup window. emitted is how many of them a signal already reported
// (rca-07); sig is the window's first signal, the template of follow-ups.
type dedupEntry struct {
	firstSeen time.Time
	lastSeen  time.Time
	count     int
	emitted   int
	pids      map[int]bool
	sig       *rca.Signal
	// participants is the wait cycle of the window's newest deadlock line
	// whose DETAIL named one, unparsed that line's unread lines (rca-07b).
	participants []rca.DeadlockParticipant
	unparsed     int
}

// Classifier matches LogEntry values against the signal pattern table and
// returns rca.Signal values with deduplication.
type Classifier struct {
	dedupWindow      time.Duration
	excludeApps      map[string]bool
	slowQueryEnabled bool
	tempFileMinBytes int64
	maxLinesPerCycle int
	// redactedStatements keeps deadlock statement text (rca-07b).
	redactedStatements bool
	logFn              func(string, string, ...any)

	dedupBuf map[string]*dedupEntry
	// pending holds follow-ups of windows closed before a drain reported
	// them (rca-07).
	pending        []*rca.Signal
	linesThisCycle int
	// cappedThisCycle counts lines skipped by maxLinesPerCycle (rca-06).
	cappedThisCycle int
	mu              sync.Mutex
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
	exclude := cfg.ExcludeApps
	if len(exclude) == 0 {
		exclude = DefaultExcludeApplications
	}
	apps := make(map[string]bool, len(exclude))
	for _, a := range exclude {
		apps[a] = true
	}
	return &Classifier{
		dedupWindow:      window,
		excludeApps:      apps,
		slowQueryEnabled: cfg.SlowQueryEnabled,
		tempFileMinBytes: cfg.TempFileMinBytes,
		maxLinesPerCycle: cfg.MaxLinesPerCycle,
		redactedStatements: cfg.DeadlockStatementText ==
			config.DeadlockStatementTextRedacted,
		logFn:    logFn,
		dedupBuf: make(map[string]*dedupEntry),
	}
}

// Classify evaluates a single LogEntry and returns a Signal if it matches
// a known pattern, or nil if no match / dedup suppresses it. The per-cycle
// line cap never skips a critical line (isCriticalLog).
func (c *Classifier) Classify(entry LogEntry) *rca.Signal {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.maxLinesPerCycle > 0 && c.linesThisCycle >= c.maxLinesPerCycle &&
		!isCriticalLog(entry.ErrorLevel, entry.SQLState) {
		c.cappedThisCycle++
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

	key := pat.ID + "\x00" + entry.Database
	if c.recordOccurrence(key, occurrencePIDs(pat.ID, entry), entry.Timestamp, sig) {
		return nil
	}
	return sig
}

// ResetCycle resets the per-cycle line counter and returns how many lines
// the cap skipped this cycle. Called by Drain().
func (c *Classifier) ResetCycle() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	capped := c.cappedThisCycle
	c.linesThisCycle, c.cappedThisCycle = 0, 0
	return capped
}

// CleanExpiredDedup removes dedup entries whose window has closed. An
// entry with repeats no signal reported yet leaves a pending follow-up.
func (c *Classifier) CleanExpiredDedup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, de := range c.dedupBuf {
		if now.Sub(de.firstSeen) >= c.dedupWindow {
			c.closeEntry(de)
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
		if matchesEntry(pat, entry, lower) {
			return pat, true
		}
	}
	return signalPattern{}, false
}

func (c *Classifier) optInEnabled(id string) bool {
	if id == signalid.LogSlowQuery {
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
	if entry.BackendType != "" {
		metrics["backend_type"] = entry.BackendType
	}
	applyExtract(pat, entry.Message, metrics)
	if !c.addSignalMetrics(pat.ID, entry, metrics) {
		return nil
	}
	return &rca.Signal{
		ID:       pat.ID,
		FiredAt:  entry.Timestamp,
		Severity: pat.Severity,
		Metrics:  metrics,
	}
}

// addSignalMetrics adds the metrics specific to signal id. It returns
// false when the signal is suppressed (a temp file below the size floor).
func (c *Classifier) addSignalMetrics(id string, entry LogEntry, metrics map[string]any) bool {
	switch id {
	case signalid.LogTempFileCreated:
		sz := parseTempFileSize(entry.Message)
		if sz < c.tempFileMinBytes {
			return false
		}
		metrics["temp_file_bytes"] = sz
	case signalid.LogStatementTimeout:
		// 57014 without the English text (lc_messages): a timeout or a
		// cancel, which the classifier cannot tell apart (rca-07).
		if !strings.Contains(strings.ToLower(entry.Message), "statement timeout") {
			metrics["message_unclassified"] = true
		}
	case signalid.LogDeadlockDetected:
		c.addDeadlockParticipants(entry, metrics)
	case signalid.LogSlowQuery:
		dur := parseSlowQueryDuration(entry.Message)
		metrics["duration_ms"] = dur
		if entry.Query != "" {
			metrics["query"] = Truncate(entry.Query, MaxQueryLen)
		}
	}
	return true
}
