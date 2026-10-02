package probes

import (
	"fmt"
	"time"
)

// TargetRow is one signal_target row: the identity and state of one
// session as it is now.
type TargetRow struct {
	PID                  int32
	BackendStart         time.Time
	QueryStart           time.Time
	XactStart            time.Time
	Database             string
	User                 string
	State                string
	BackendType          string
	QueryHash            string
	ApplicationHash      string
	QueryID              int64
	Waiting              bool
	Blocking             int
	InRecovery           bool
	InCurrentDatabase    bool
	ProtectedApplication bool
}

// SignalTargets decodes a signal_target result (empty: the session is
// gone or is another incarnation of the pid).
func SignalTargets(res Result) ([]TargetRow, error) {
	rows, err := rowsFor(res, SignalTarget)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]TargetRow, 0, len(rows))
	for i, r := range rows {
		pid, err := intField(r, "pid")
		if err != nil {
			return nil, fmt.Errorf("signal_target row %d: %w", i+1, err)
		}
		start := timeField(r, "backend_start")
		if start.IsZero() {
			return nil, fmt.Errorf("signal_target row %d: backend_start is unreadable", i+1)
		}
		blocking, _ := intField(r, "blocking")
		out = append(out, TargetRow{PID: int32(pid), BackendStart: start,
			QueryStart: timeField(r, "query_start"), XactStart: timeField(r, "xact_start"),
			Database: strField(r, "datname"), User: strField(r, "usename"),
			State: strField(r, "state"), BackendType: strField(r, "backend_type"),
			QueryHash: strField(r, "query_hash"), QueryID: optionalInt(r, "query_id"),
			ApplicationHash: strField(r, "application_hash"),
			Waiting:         boolField(r, "waiting"), Blocking: int(blocking),
			InRecovery:           boolField(r, "in_recovery"),
			InCurrentDatabase:    boolField(r, "in_current_database"),
			ProtectedApplication: boolField(r, "protected_application")})
	}
	return out, nil
}

// optionalInt is an integer column that may be NULL (0).
func optionalInt(r Row, key string) int64 {
	n, err := intField(r, key)
	if err != nil {
		return 0
	}
	return n
}

// RecoveryRow is one recovery_sample row: one client session of this
// database.
type RecoveryRow struct {
	PID             int32
	BackendStart    time.Time
	State           string
	Waiting         bool
	BlockedByTarget bool
	IsTarget        bool
}

// RecoveryRows decodes a recovery_sample result.
func RecoveryRows(res Result) ([]RecoveryRow, error) {
	rows, err := rowsFor(res, RecoverySample)
	if err != nil {
		return nil, err
	}
	out := make([]RecoveryRow, 0, len(rows))
	for i, r := range rows {
		pid, err := intField(r, "pid")
		if err != nil {
			return nil, fmt.Errorf("recovery_sample row %d: %w", i+1, err)
		}
		out = append(out, RecoveryRow{PID: int32(pid),
			BackendStart: timeField(r, "backend_start"), State: strField(r, "state"),
			Waiting: boolField(r, "waiting"), BlockedByTarget: boolField(r, "blocked_by_target"),
			IsTarget: boolField(r, "is_target")})
	}
	return out, nil
}

// EdgeIdentity is a lock_graph edge with the waiter's session identity
// and the blocker's query id: what an action derives its target and its
// recovery baseline from.
type EdgeIdentity struct {
	LockEdge
	WaiterBackendStart time.Time
	BlockerQueryID     int64
}

// LockEdgeIdentities decodes a lock_graph result with identities.
func LockEdgeIdentities(res Result) ([]EdgeIdentity, error) {
	edges, err := LockEdges(res)
	if err != nil || len(edges) == 0 {
		return nil, err
	}
	out := make([]EdgeIdentity, 0, len(edges))
	for i, e := range edges {
		r := res.Rows[i]
		out = append(out, EdgeIdentity{LockEdge: e,
			WaiterBackendStart: timeField(r, "waiter_backend_start"),
			BlockerQueryID:     optionalInt(r, "blocker_query_id")})
	}
	return out, nil
}
