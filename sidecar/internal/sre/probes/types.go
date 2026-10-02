// Package probes is the Sage SRE probe registry (AI-SRE-SPEC §5, Codex
// §8): a fixed, versioned catalog of read-only diagnostic queries. Every
// probe has hard row, byte and time caps, runs in a read-only
// transaction with a fixed search_path, statement_timeout and
// lock_timeout, and returns a typed result: ok, empty, error,
// no_privilege or unsupported. An error is never reported as healthy.
// Callers (and the LLM) name a catalog id; they never supply SQL.
package probes

import (
	"errors"
	"fmt"
	"time"
)

// ID names a catalog probe.
type ID string

// R1 catalog probes.
const (
	LockChains           ID = "lock_chains"
	LockGraph            ID = "lock_graph"
	LongTransactions     ID = "long_transactions"
	PreparedXacts        ID = "prepared_xacts"
	BackendIdentity      ID = "backend_identity"
	ConnectionSaturation ID = "connection_saturation"
	ReplicationLag       ID = "replication_lag"
	ReplicationSlots     ID = "replication_slots"
	WALCheckpoint        ID = "wal_checkpoint"
	AutovacuumWraparound ID = "autovacuum_wraparound"
	VacuumProgress       ID = "vacuum_progress"
	PlanRegressions      ID = "plan_regressions"
	// M2: archiver health and pg_sage's own recent actions.
	Archiver    ID = "archiver"
	SageActions ID = "sage_actions"
	// M6 reactive families: checkpoint storms, temp-file explosions,
	// replication lag (standby side) and LWLock contention.
	CheckpointActivity  ID = "checkpoint_activity"
	TempFileActivity    ID = "temp_file_activity"
	TempFileHolders     ID = "temp_file_holders"
	TempSpillStatements ID = "temp_spill_statements"
	StandbyReplayState  ID = "standby_replay_state"
	LWLockWaits         ID = "lwlock_waits"
)

// Probe families.
const (
	FamilyLocks       = "lock_blocking"
	FamilyConnections = "connection_pressure"
	FamilyReplication = "replication"
	FamilyWAL         = "wal_checkpoint"
	FamilyVacuum      = "vacuum_wraparound"
	FamilyPlans       = "plan_regression"
	FamilyChange      = "change"
	FamilyTempFiles   = "temp_files"
	FamilyWaits       = "wait_events"
)

// ExtSchemaToken stands for the schema of a spec's Extension in its SQL;
// the runner replaces it with the quoted schema the extension lives in.
const ExtSchemaToken = "@extschema@"

// Hard ceilings (Codex §8). A spec may be stricter, never looser.
const (
	MaxStatementTimeout   = 500 * time.Millisecond
	MaxRows               = 500
	MaxBytes              = 256 << 10
	MaxSidecarConcurrency = 4
	// MaxProbesPerSignal bounds the probes one incident signal runs.
	MaxProbesPerSignal = 4
	// MinWindow, MaxWindow and DefaultWindow bound ArgsWindow probes.
	MinWindow     = time.Minute
	MaxWindow     = 7 * 24 * time.Hour
	DefaultWindow = 24 * time.Hour
	// queueWait bounds how long a probe waits for a concurrency slot.
	queueWait = 5 * time.Second
)

// Status is the typed outcome of one probe run.
type Status string

// Probe statuses.
const (
	StatusOK          Status = "ok"
	StatusEmpty       Status = "empty"
	StatusError       Status = "error"
	StatusNoPrivilege Status = "no_privilege"
	StatusUnsupported Status = "unsupported"
)

// Usable reports whether the result is an observation (rows or an
// observed absence of rows) rather than a failure to observe.
func (s Status) Usable() bool { return s == StatusOK || s == StatusEmpty }

// ArgKind is the typed argument shape a probe accepts.
type ArgKind int

// Argument shapes.
const (
	ArgsNone ArgKind = iota
	ArgsBackend
	ArgsWindow
)

// Args are typed probe arguments. Backend identity pins one session
// (pid plus backend_start, since PIDs are reused); Window bounds a
// history lookback (zero means DefaultWindow).
type Args struct {
	PID          int32
	BackendStart time.Time
	Window       time.Duration
}

func (a Args) validate(kind ArgKind) error {
	switch kind {
	case ArgsNone:
		if a != (Args{}) {
			return errors.New("probe takes no arguments")
		}
	case ArgsBackend:
		if a.PID <= 0 || a.BackendStart.IsZero() || a.Window != 0 {
			return errors.New("probe needs a pid and backend_start")
		}
	case ArgsWindow:
		if a.PID != 0 || !a.BackendStart.IsZero() {
			return errors.New("probe takes only a window")
		}
		if a.Window != 0 && (a.Window < MinWindow || a.Window > MaxWindow) {
			return fmt.Errorf("window %s outside [%s, %s]", a.Window,
				MinWindow, MaxWindow)
		}
	default:
		return fmt.Errorf("unknown argument kind %d", kind)
	}
	return nil
}

// params builds the positional parameters: $1 is always the row limit
// (max rows + 1, so truncation is detectable), then the typed args.
func (a Args) params(kind ArgKind, limit int) []any {
	switch kind {
	case ArgsBackend:
		return []any{limit, a.PID, a.BackendStart}
	case ArgsWindow:
		w := a.Window
		if w == 0 {
			w = DefaultWindow
		}
		return []any{limit, w.Seconds()}
	default:
		return []any{limit}
	}
}

// Variant is the SQL used from MinVersion (server_version_num) upward.
type Variant struct {
	MinVersion int
	SQL        string
}

// Spec declares one catalog probe.
type Spec struct {
	ID          ID
	Version     string
	Family      string
	Description string
	Args        ArgKind
	Variants    []Variant // ascending MinVersion
	// Extension names the extension whose objects the SQL reads through
	// ExtSchemaToken; the probe is unsupported where it is not installed.
	Extension        string
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	MaxRows          int
	MaxBytes         int
}

// VariantFor returns the SQL variant for a server version.
func (s Spec) VariantFor(version int) (Variant, bool) {
	var out Variant
	found := false
	for _, v := range s.Variants {
		if version > 0 && version >= v.MinVersion {
			out, found = v, true
		}
	}
	return out, found
}

// Row is one result row keyed by column name. Integers are int64,
// floating point values float64, timestamps time.Time (UTC).
type Row map[string]any

// Result is the typed outcome of one probe run.
type Result struct {
	ProbeID    ID        `json:"probe_id"`
	Version    string    `json:"probe_version"`
	Status     Status    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	ElapsedMS  int64     `json:"elapsed_ms"`
	Columns    []string  `json:"columns,omitempty"`
	Rows       []Row     `json:"rows,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
}

// UnavailableError reports a probe result that is not an observation.
type UnavailableError struct {
	ProbeID ID
	Status  Status
	Reason  string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("probe %s %s: %s", e.ProbeID, e.Status, e.Reason)
}
