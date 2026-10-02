package probes

import (
	"fmt"
	"math"
	"time"
)

// M2 decoders. Unknown numbers are NaN, never zero; an unavailable
// result is an *UnavailableError.

// ConnGroup is one connection_saturation row: backends of one
// application, client address and state.
type ConnGroup struct {
	InCurrentDatabase   bool
	Application         string
	ClientAddr          string
	State               string
	Backends            int64
	WaitingOnLock       int64
	MaxConnections      int64
	ReservedConnections int64
	TotalClientBackends int64
	ServerStartedAt     time.Time
	Identity            ServerIdentity
}

// Slot is one replication slot.
type Slot struct {
	Name          string
	Type          string
	Active        bool
	WALStatus     string
	RetainedBytes float64
	SafeWALSize   float64
	Database      string
	InactiveSince time.Time
	Identity      ServerIdentity
}

// WALStat is the cumulative WAL volume since its statistics reset.
type WALStat struct {
	WALBytes   float64
	StatsReset time.Time
	Identity   ServerIdentity
}

// ArchiverStat is the WAL archiver's state.
type ArchiverStat struct {
	Mode           string
	Archived       float64
	Failed         float64
	LastArchivedAt time.Time
	LastFailedAt   time.Time
	StatsReset     time.Time
}

// SageAction is one of pg_sage's own recent actions.
type SageAction struct {
	ID         int64
	ActionType string
	Outcome    string
	ExecutedAt time.Time
	AgeS       float64
}

// ConnectionGroups decodes a connection_saturation result.
func ConnectionGroups(res Result) ([]ConnGroup, error) {
	rows, err := rowsFor(res, ConnectionSaturation)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]ConnGroup, 0, len(rows))
	for i, r := range rows {
		n, err := intField(r, "backends")
		if err != nil {
			return nil, fmt.Errorf("connection_saturation row %d: %w", i+1, err)
		}
		waiting, _ := intField(r, "waiting_on_lock")
		maxConn, _ := intField(r, "max_connections")
		reserved, _ := intField(r, "reserved_connections")
		total, _ := intField(r, "total_client_backends")
		out = append(out, ConnGroup{InCurrentDatabase: boolField(r, "in_current_database"),
			Application: strField(r, "application_name"),
			ClientAddr:  strField(r, "client_addr"), State: strField(r, "state"),
			Backends: n, WaitingOnLock: waiting, MaxConnections: maxConn,
			ReservedConnections: reserved, TotalClientBackends: total,
			ServerStartedAt: timeField(r, "server_started_at"), Identity: IdentityOf(r)})
	}
	return out, nil
}

// Slots decodes a replication_slots result.
func Slots(res Result) ([]Slot, error) {
	rows, err := rowsFor(res, ReplicationSlots)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]Slot, 0, len(rows))
	for _, r := range rows {
		out = append(out, Slot{Name: strField(r, "slot_name"),
			Type: strField(r, "slot_type"), Active: boolField(r, "active"),
			WALStatus:     strField(r, "wal_status"),
			RetainedBytes: floatField(r, "retained_bytes"),
			SafeWALSize:   floatField(r, "safe_wal_size"),
			Database:      strField(r, "database"),
			InactiveSince: timeField(r, "inactive_since"), Identity: IdentityOf(r)})
	}
	return out, nil
}

// WALStats decodes the WAL volume of a wal_checkpoint result. A result
// without its row is unavailable, not zero.
func WALStats(res Result) (WALStat, error) {
	r, err := singleRow(res, WALCheckpoint)
	if err != nil {
		return WALStat{}, err
	}
	return WALStat{WALBytes: floatField(r, "wal_bytes"),
		StatsReset: timeField(r, "wal_stats_reset"), Identity: IdentityOf(r)}, nil
}

// ArchiverStats decodes an archiver result.
func ArchiverStats(res Result) (ArchiverStat, error) {
	r, err := singleRow(res, Archiver)
	if err != nil {
		return ArchiverStat{}, err
	}
	return ArchiverStat{Mode: strField(r, "archive_mode"),
		Archived: floatField(r, "archived_count"), Failed: floatField(r, "failed_count"),
		LastArchivedAt: timeField(r, "last_archived_time"),
		LastFailedAt:   timeField(r, "last_failed_time"),
		StatsReset:     timeField(r, "stats_reset")}, nil
}

// SageActionRows decodes a sage_actions result; an empty result is an
// observed absence of actions (nil, no error).
func SageActionRows(res Result) ([]SageAction, error) {
	rows, err := rowsFor(res, SageActions)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]SageAction, 0, len(rows))
	for i, r := range rows {
		id, err := intField(r, "id")
		if err != nil {
			return nil, fmt.Errorf("sage_actions row %d: %w", i+1, err)
		}
		out = append(out, SageAction{ID: id, ActionType: strField(r, "action_type"),
			Outcome: strField(r, "outcome"), ExecutedAt: timeField(r, "executed_at"),
			AgeS: floatField(r, "age_s")})
	}
	return out, nil
}

// singleRow returns the one row of a single-row probe.
func singleRow(res Result, id ID) (Row, error) {
	rows, err := rowsFor(res, id)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, &UnavailableError{ProbeID: id, Status: res.Status,
			Reason: fmt.Sprintf("expected one row, got %d", len(rows))}
	}
	return rows[0], nil
}

// Known reports a decoded number that is not unknown.
func Known(v float64) bool { return !math.IsNaN(v) }
