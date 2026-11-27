package logwatch

import (
	"errors"
	"time"
)

// Log-source states (rca-09).
const (
	SourceActive      = "active"      // the watcher is reading the server log
	SourceUnavailable = "unavailable" // wanted, but the log cannot be read
	SourceDisabled    = "disabled"    // switched off by configuration
)

// ErrLogSourceUnsupported marks a server whose log settings produce no file
// pg_sage can read; the message names the setting to change.
var ErrLogSourceUnsupported = errors.New("logwatch: server log is not readable")

// SourceStatus is what a database's log-based RCA can see: the state, the
// setting at fault when it is not active, and the watcher's progress.
type SourceStatus struct {
	State        string     `json:"state"`
	Reason       string     `json:"reason,omitempty"`
	Format       string     `json:"format,omitempty"`
	File         string     `json:"file,omitempty"`
	LastLineAt   *time.Time `json:"last_line_at,omitempty"`
	ParseErrors  uint64     `json:"parse_errors"`
	DroppedLines uint64     `json:"dropped_lines"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	Hints        []string   `json:"hints,omitempty"`
}

// DisabledStatus is a source switched off; reason names the setting.
func DisabledStatus(reason string) SourceStatus {
	return SourceStatus{State: SourceDisabled, Reason: reason}
}

// UnavailableStatus is a wanted source that cannot be read.
func UnavailableStatus(reason string) SourceStatus {
	if reason == "" {
		reason = "the server log could not be read"
	}
	return SourceStatus{State: SourceUnavailable, Reason: reason}
}

// OrDisabled returns s, or a disabled status with reason when s was never
// set, so callers never report a zero-value status.
func (s SourceStatus) OrDisabled(reason string) SourceStatus {
	if s.State == "" {
		return DisabledStatus(reason)
	}
	return s
}

// Active reports whether the watcher has started and is reading the log.
func (fw *FileWatcher) Active() bool {
	fw.stateMu.RLock()
	defer fw.stateMu.RUnlock()
	return fw.started
}

// Active reports whether the fanout's watcher is reading the log; a fanout
// handed out while its source is unavailable is not.
func (f *LogFanout) Active() bool {
	return f.source.Active()
}

// Status reports the watcher's own state: active once started, with the
// file it reads, the time it last read a line and its error counts. Safe
// to call while Drain runs.
func (fw *FileWatcher) Status() SourceStatus {
	fw.stateMu.RLock()
	tailer := fw.tailer
	started := fw.started
	fw.stateMu.RUnlock()
	if !started || tailer == nil {
		return UnavailableStatus("log watcher not started")
	}
	drops := fw.drops.snapshot()
	st := SourceStatus{State: SourceActive, Format: tailer.format,
		File: tailer.CurrentPath(), ParseErrors: fw.parseErrors.Load(),
		DroppedLines: drops.Total()}
	if nanos := fw.lastLine.Load(); nanos != 0 {
		at := time.Unix(0, nanos)
		st.LastLineAt = &at
	}
	return st
}
