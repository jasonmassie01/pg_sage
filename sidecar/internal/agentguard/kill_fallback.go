package agentguard

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// FallbackLog is the kill switch's local append-only audit (§6.2.5): when
// the gate or the control database is unreachable, containment runs
// directly and each step is written here, one JSON object per line, then
// reconciled into sage.action_log when the database is back.
type FallbackLog struct {
	path string
	mu   sync.Mutex
}

// FallbackEntry is one directly executed kill step.
type FallbackEntry struct {
	At          time.Time `json:"at"`
	ActionType  string    `json:"action_type"`
	Database    string    `json:"database"`
	Scope       string    `json:"scope,omitempty"`
	Target      string    `json:"target,omitempty"`
	PrincipalID string    `json:"principal_id,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	Statements  []string  `json:"statements,omitempty"`
	Outcome     string    `json:"outcome"`
	Error       string    `json:"error,omitempty"`
}

// NewFallbackLog returns a log at path, or nil when path is empty.
func NewFallbackLog(path string) *FallbackLog {
	if path == "" {
		return nil
	}
	return &FallbackLog{path: path}
}

// maxFallbackLine bounds one entry when reading the log back.
const maxFallbackLine = 4 << 20

// Append writes e as one line and syncs it to disk.
func (l *FallbackLog) Append(e FallbackEntry) error {
	if l == nil {
		return ErrNoFallbackLog
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("agentguard: encoding fallback entry: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("agentguard: opening kill fallback log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("agentguard: writing kill fallback log: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("agentguard: syncing kill fallback log: %w", err)
	}
	return f.Close()
}

// Entries reads every entry; a log never written to has none.
func (l *FallbackLog) Entries() ([]FallbackEntry, error) {
	if l == nil {
		return nil, ErrNoFallbackLog
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readLocked()
}

func (l *FallbackLog) readLocked() ([]FallbackEntry, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return []FallbackEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agentguard: opening kill fallback log: %w", err)
	}
	defer func() { _ = f.Close() }()
	out := []FallbackEntry{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxFallbackLine)
	for sc.Scan() {
		var e FallbackEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("agentguard: kill fallback log line %d: %w",
				len(out)+1, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("agentguard: reading kill fallback log: %w", err)
	}
	return out, nil
}

// drain hands every entry to record, then moves the log aside so no entry
// is recorded twice. A record failure keeps the log for the next try.
func (l *FallbackLog) drain(record func([]FallbackEntry) error) (int, error) {
	if l == nil {
		return 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, err := l.readLocked()
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	if err := record(entries); err != nil {
		return 0, err
	}
	done := fmt.Sprintf("%s.reconciled-%d", l.path, time.Now().UnixNano())
	if err := os.Rename(l.path, done); err != nil {
		return 0, fmt.Errorf("agentguard: entries were recorded but the kill fallback "+
			"log could not be moved aside (they may be recorded again): %w", err)
	}
	return len(entries), nil
}
