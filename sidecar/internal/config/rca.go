package config

import (
	"fmt"
	"time"
)

// Bounds for rca.lock_chain_interval_seconds. 0 disables the fast path.
const (
	minRCALockChainIntervalSeconds = 10
	maxRCALockChainIntervalSeconds = 3600
	maxRCAStaleAfterHours          = 8760

	// Bounds for rca.log_fast_path_interval_seconds. 0 disables it.
	minRCALogFastPathIntervalSeconds = 1
	maxRCALogFastPathIntervalSeconds = 3600

	// Bounds for rca.log_quiet_minutes (0 = the dedup window) and
	// rca.recurrence_renotify_hours (rca-07).
	maxRCALogQuietMinutes         = 1440
	maxRCARecurrenceRenotifyHours = 168
)

// Values of rca.deadlock_statement_text (rca-07b). Fingerprint keeps a
// hash of each deadlock participant's literal-redacted statement and no
// text; redacted also keeps the redacted text. Any other value keeps no
// text: a config that bypassed validation fails closed.
const (
	DeadlockStatementTextFingerprint = "fingerprint"
	DeadlockStatementTextRedacted    = "redacted"
	DefaultRCADeadlockStatementText  = DeadlockStatementTextFingerprint
)

// validateDeadlockStatementText accepts the two modes, and empty (a config
// built without the key) as fingerprint.
func (r *RCAConfig) validateDeadlockStatementText() error {
	switch r.DeadlockStatementText {
	case "", DeadlockStatementTextFingerprint, DeadlockStatementTextRedacted:
		return nil
	}
	return fmt.Errorf("rca.deadlock_statement_text must be %q or %q, got %q",
		DeadlockStatementTextFingerprint, DeadlockStatementTextRedacted,
		r.DeadlockStatementText)
}

// LogQuiet is how long a log incident must go without an occurrence
// before it auto-resolves: rca.log_quiet_minutes, else the dedup window,
// else its 30 min default. A config built without the keys never gets 0.
func (r *RCAConfig) LogQuiet() time.Duration {
	switch {
	case r.LogQuietMinutes > 0:
		return time.Duration(r.LogQuietMinutes) * time.Minute
	case r.DedupWindowMinutes > 0:
		return time.Duration(r.DedupWindowMinutes) * time.Minute
	}
	return DefaultRCADedupWindowMinutes * time.Minute
}

// RecurrenceRenotify is the window after a log incident's resolution in
// which a recurrence is not paged again; a zero config gets the default.
func (r *RCAConfig) RecurrenceRenotify() time.Duration {
	if r.RecurrenceRenotifyHours <= 0 {
		return DefaultRCARecurrenceRenotifyHours * time.Hour
	}
	return time.Duration(r.RecurrenceRenotifyHours) * time.Hour
}

// LockChainInterval is the lock-chain fast-path period; zero means the
// fast path is disabled.
func (r *RCAConfig) LockChainInterval() time.Duration {
	if r.LockChainIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(r.LockChainIntervalSeconds) * time.Second
}

// LogFastPathInterval is the log fast-path period; zero means the log fast
// path is disabled.
func (r *RCAConfig) LogFastPathInterval() time.Duration {
	if r.LogFastPathIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(r.LogFastPathIntervalSeconds) * time.Second
}

// StaleAfter is how long an open incident may go without being
// re-detected before it resolves as stale. A config built without the
// key (zero) gets the default, never "resolve immediately".
func (r *RCAConfig) StaleAfter() time.Duration {
	if r.StaleAfterHours <= 0 {
		return DefaultRCAStaleAfterHours * time.Hour
	}
	return time.Duration(r.StaleAfterHours) * time.Hour
}

func (r *RCAConfig) validate() error {
	if err := r.validateStaleAfter(); err != nil {
		return err
	}
	if err := r.validateVacuumFloor(); err != nil {
		return err
	}
	if err := r.validateHolderFloor(); err != nil {
		return err
	}
	if err := r.validateLogFastPath(); err != nil {
		return err
	}
	if err := r.validateLogQuiet(); err != nil {
		return err
	}
	if err := r.validateDeadlockStatementText(); err != nil {
		return err
	}
	s := r.LockChainIntervalSeconds
	if s == 0 {
		return nil
	}
	if s < minRCALockChainIntervalSeconds || s > maxRCALockChainIntervalSeconds {
		return fmt.Errorf("rca.lock_chain_interval_seconds must be 0 "+
			"(disabled) or %d-%d, got %d", minRCALockChainIntervalSeconds,
			maxRCALockChainIntervalSeconds, s)
	}
	return nil
}

func (r *RCAConfig) validateStaleAfter() error {
	h := r.StaleAfterHours
	if h < 1 || h > maxRCAStaleAfterHours {
		return fmt.Errorf("rca.stale_after_hours must be 1-%d, got %d",
			maxRCAStaleAfterHours, h)
	}
	if h*60 < r.DedupWindowMinutes {
		return fmt.Errorf("rca.stale_after_hours (%d h) must cover "+
			"rca.dedup_window_minutes (%d)", h, r.DedupWindowMinutes)
	}
	return nil
}

func (r *RCAConfig) validateLogFastPath() error {
	s := r.LogFastPathIntervalSeconds
	if s == 0 {
		return nil
	}
	if s < minRCALogFastPathIntervalSeconds || s > maxRCALogFastPathIntervalSeconds {
		return fmt.Errorf("rca.log_fast_path_interval_seconds must be 0 "+
			"(disabled) or %d-%d, got %d", minRCALogFastPathIntervalSeconds,
			maxRCALogFastPathIntervalSeconds, s)
	}
	return nil
}

// validateLogQuiet checks the rca-07 keys. A quiet period longer than the
// stale window would never apply: the incident resolves as stale first.
func (r *RCAConfig) validateLogQuiet() error {
	q := r.LogQuietMinutes
	if q < 0 || q > maxRCALogQuietMinutes {
		return fmt.Errorf("rca.log_quiet_minutes must be 0 (the dedup window) "+
			"or 1-%d, got %d", maxRCALogQuietMinutes, q)
	}
	if q > r.StaleAfterHours*60 {
		return fmt.Errorf("rca.log_quiet_minutes (%d) must not exceed "+
			"rca.stale_after_hours (%d h)", q, r.StaleAfterHours)
	}
	h := r.RecurrenceRenotifyHours
	if h < 1 || h > maxRCARecurrenceRenotifyHours {
		return fmt.Errorf("rca.recurrence_renotify_hours must be 1-%d, got %d",
			maxRCARecurrenceRenotifyHours, h)
	}
	return nil
}
