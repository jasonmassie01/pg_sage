package logwatch

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// signalPattern defines one pattern the classifier matches against log entries.
//
// A pattern with Regex matches when the message matches it and, if
// SQLStates is set, the SQLSTATE is one of them. Older patterns match on
// SQLSTATE or, failing that, on Substrings. The regexes come from the
// captured corpus under testdata/ (scripts/capture_logwatch_corpus.py),
// not from memory: every text rule needs lc_messages C or English.
type signalPattern struct {
	ID         string
	Severity   string
	SQLStates  []string // exact match (or prefix for XX/28)
	Substrings []string // case-insensitive, ALL must match
	MinLevel   string   // minimum ErrorLevel
	OptIn      bool     // requires explicit config enable
	Regex      *regexp.Regexp
	// Extract copies Regex submatches into the signal's metrics.
	Extract func(m []string, metrics map[string]any)
	// Origin restricts which process may have logged the line.
	Origin string
}

const (
	// originServer rejects lines a client backend logged, so a client's
	// RAISE of the same text cannot fake a server event.
	originServer = "server"
	// originPostmaster accepts only the postmaster's own lines.
	originPostmaster = "postmaster"
)

var (
	// PG14-17 say "server process (PID n)" for backends and name auxiliary
	// processes ("WAL writer process", "checkpointer process"); PG18 names
	// the backend type; background workers say `background worker "name"`.
	reCrash = regexp.MustCompile(
		`^([A-Za-z][A-Za-z ]*(?: "[^"]*")?) \(PID (\d+)\) was terminated by signal (\d+)`)
	// Logged by the walsender (ERROR) and quoted by the standby's
	// walreceiver (FATAL "could not receive data from WAL stream: ...").
	reWALRemoved = regexp.MustCompile(
		`requested WAL segment ([0-9A-F]{24}) has already been removed`)
	// PG14-15: invalidating slot "s" because its restart_lsn ... exceeds
	// max_slot_wal_keep_size. PG16+: invalidating obsolete replication slot "s".
	reSlotInvalidated = regexp.MustCompile(
		`^invalidating (?:obsolete replication )?slot "([^"]+)"`)
	// PG14-16 omit "that assign new transaction IDs"; PG17+ include it.
	reXIDOutage = regexp.MustCompile(
		`is not accepting commands (?:that assign new transaction IDs )?` +
			`to avoid wraparound data loss in database ` +
			`(?:"([^"]*)"|with OID (\d+))`)
	// PG14-16 say "generate", PG17+ "assign"; the members limit is separate.
	reMultiXactOutage = regexp.MustCompile(
		`is not accepting commands that (?:generate|assign) new MultiXactIds ` +
			`to avoid wraparound data loss|^multixact "members" limit exceeded`)
	reMultiXactWarning = regexp.MustCompile(
		`must be vacuumed before \d+ more (?:MultiXactIds?|multixact members?) ` +
			`(?:is|are) used`)
)

// patterns is the catalog of signal definitions, matched in order.
var patterns = []signalPattern{
	{ID: signalid.LogDeadlockDetected, Severity: "critical",
		SQLStates: []string{"40P01"}, MinLevel: "ERROR"},
	{ID: signalid.LogConnectionRefused, Severity: "critical",
		SQLStates: []string{"53300"}, MinLevel: "FATAL"},
	{ID: signalid.LogOutOfMemory, Severity: "critical",
		SQLStates: []string{"53200"}, MinLevel: "ERROR"},
	{ID: signalid.LogDiskFull, Severity: "critical",
		SQLStates: []string{"53100"}, MinLevel: "ERROR"},
	{ID: signalid.LogPanicServerCrash, Severity: "critical", MinLevel: "LOG",
		Regex: reCrash, Extract: extractCrash, Origin: originPostmaster},
	// Before the XX rules: the standby's walreceiver may log the quoted
	// walsender error under a generic SQLSTATE such as XX000.
	{ID: signalid.LogWALSegmentRemoved, Severity: "critical", MinLevel: "ERROR",
		Regex: reWALRemoved, Extract: extractWALSegment, Origin: originServer},
	{ID: signalid.LogDataCorruption, Severity: "critical",
		MinLevel: "WARNING"},
	{ID: signalid.LogInternalError, Severity: "warning",
		SQLStates: []string{"XX000"}, MinLevel: "WARNING"},
	{ID: signalid.LogWraparoundOutage, Severity: "critical", MinLevel: "ERROR",
		SQLStates: []string{"54000"}, Regex: reXIDOutage,
		Extract: extractWraparoundDB},
	{ID: signalid.LogMultiXactWraparoundOutage, Severity: "critical",
		MinLevel: "ERROR", SQLStates: []string{"54000"},
		Regex: reMultiXactOutage},
	{ID: signalid.LogMultiXactWraparoundWarning, Severity: "critical",
		MinLevel: "WARNING", Regex: reMultiXactWarning},
	{ID: signalid.LogTxidWraparoundWarning, Severity: "critical",
		Substrings: []string{"must be vacuumed within"},
		MinLevel:   "WARNING"},
	{ID: signalid.LogArchiveFailed, Severity: "critical",
		Substrings: []string{"archive command failed"}, MinLevel: "LOG"},
	{ID: signalid.LogTempFileCreated, Severity: "warning",
		Substrings: []string{"temporary file"}, MinLevel: "LOG"},
	{ID: signalid.LogCheckpointTooFrequent, Severity: "warning",
		Substrings: []string{"checkpoints are occurring too frequently"},
		MinLevel:   "LOG"},
	{ID: signalid.LogLockTimeout, Severity: "warning",
		SQLStates: []string{"55P03"}, MinLevel: "ERROR"},
	// rca-07: 57014 is also a pg_cancel_backend or client cancel; that is
	// info and opens no incident. Without English text it stays a timeout.
	{ID: signalid.LogStatementCancelled, Severity: "info",
		SQLStates: []string{"57014"}, MinLevel: "ERROR"},
	{ID: signalid.LogStatementTimeout, Severity: "warning",
		SQLStates: []string{"57014"}, MinLevel: "ERROR"},
	{ID: signalid.LogReplicationConflict, Severity: "warning",
		Substrings: []string{"conflict with recovery"},
		MinLevel:   "ERROR"},
	{ID: signalid.LogAutovacuumCancel, Severity: "warning",
		Substrings: []string{"canceling autovacuum task"},
		MinLevel:   "LOG"},
	{ID: signalid.LogReplicationSlotInactive, Severity: "warning", MinLevel: "LOG",
		Regex: reSlotInvalidated, Extract: extractSlotName, Origin: originServer},
	{ID: signalid.LogAuthenticationFailure, Severity: "warning",
		MinLevel: "FATAL"},
	{ID: signalid.LogSlowQuery, Severity: "info",
		Substrings: []string{"duration:"}, MinLevel: "LOG", OptIn: true},
}

// matchesEntry reports whether pat matches entry. lower is the lowered
// message, for substring rules.
func matchesEntry(pat signalPattern, entry LogEntry, lower string) bool {
	switch pat.ID {
	case "log_panic_server_crash":
		return entry.ErrorLevel == "PANIC" || (entry.ErrorLevel == "LOG" &&
			originAllows(pat.Origin, entry) && pat.Regex.MatchString(entry.Message))
	case "log_data_corruption":
		// XX001 and XX002 are corruption; an unknown future XX code fails
		// closed to corruption too. XX000 is log_internal_error.
		return strings.HasPrefix(entry.SQLState, "XX") && entry.SQLState != "XX000"
	case "log_authentication_failure":
		return strings.HasPrefix(entry.SQLState, "28")
	case "log_statement_cancelled":
		return entry.SQLState == "57014" && strings.Contains(lower, "due to user request")
	case "log_statement_timeout":
		// An autovacuum worker cancelled for a lock logs 57014 at ERROR; the
		// autovacuum-cancel rule below takes it.
		return entry.SQLState == "57014" &&
			!strings.Contains(lower, "canceling autovacuum task")
	}
	if pat.Regex != nil {
		return originAllows(pat.Origin, entry) &&
			(len(pat.SQLStates) == 0 || sqlStateIn(entry.SQLState, pat.SQLStates)) &&
			pat.Regex.MatchString(entry.Message)
	}
	if entry.SQLState != "" && sqlStateIn(entry.SQLState, pat.SQLStates) {
		return true
	}
	if len(pat.Substrings) > 0 {
		return allSubstringsMatch(lower, pat.Substrings)
	}
	return false
}

func sqlStateIn(state string, states []string) bool {
	for _, s := range states {
		if state == s {
			return true
		}
	}
	return false
}

// originAllows checks which process logged the line. When backend_type
// is absent (pre-PG13 csvlog, some provider sinks), a line with no user
// and no database is taken to be the postmaster's.
func originAllows(origin string, entry LogEntry) bool {
	switch origin {
	case originPostmaster:
		if entry.BackendType != "" {
			return entry.BackendType == "postmaster"
		}
		return entry.User == "" && entry.Database == ""
	case originServer:
		return entry.BackendType != "client backend"
	default:
		return true
	}
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

// applyExtract fills pattern-specific metrics from the message.
func applyExtract(pat signalPattern, msg string, metrics map[string]any) {
	if pat.Regex == nil || pat.Extract == nil {
		return
	}
	if m := pat.Regex.FindStringSubmatch(msg); m != nil {
		pat.Extract(m, metrics)
	}
}

func extractCrash(m []string, metrics map[string]any) {
	metrics["process_type"] = m[1]
	if pid, err := strconv.Atoi(m[2]); err == nil {
		metrics["crashed_pid"] = pid
	}
	if sig, err := strconv.Atoi(m[3]); err == nil {
		metrics["signal"] = sig
	}
}

func extractWALSegment(m []string, metrics map[string]any) {
	metrics["wal_segment"] = m[1]
}

func extractSlotName(m []string, metrics map[string]any) {
	metrics["slot_name"] = m[1]
}

func extractWraparoundDB(m []string, metrics map[string]any) {
	if m[1] != "" {
		metrics["wraparound_database"] = m[1]
		return
	}
	metrics["wraparound_database"] = m[2]
}
