package probes

import (
	"encoding/json"
	"strconv"
	"time"
)

// Server identity (CHECK-07): every probe a two-sample investigation
// compares also reads which server incarnation answered, so a restart,
// a failover or another server behind the same address between the
// samples is detected instead of compared.

// Server roles.
const (
	ServerRolePrimary = "primary"
	ServerRoleStandby = "standby"
)

// serverIdentityColumns are the identity columns, appended to a probe's
// select list. On a primary the timeline is the WAL insert timeline
// (current immediately after a promotion); a standby reports the
// timeline of its last checkpoint. Every function is executable by
// PUBLIC.
const serverIdentityColumns = `pg_catalog.pg_postmaster_start_time() AS server_started_at,
       (SELECT c.system_identifier::text FROM pg_catalog.pg_control_system() c)
           AS system_identifier,
       CASE WHEN pg_catalog.pg_is_in_recovery()
            THEN (SELECT c.timeline_id::int8 FROM pg_catalog.pg_control_checkpoint() c)
            ELSE ('x' || pg_catalog.substr(pg_catalog.pg_walfile_name(
                     pg_catalog.pg_current_wal_lsn()), 1, 8))::bit(32)::int8
       END AS timeline_id,
       pg_catalog.pg_is_in_recovery() AS in_recovery,
       pg_catalog.current_setting('server_version_num')::int8 AS server_version_num`

// ServerIdentity is the server incarnation that produced a sample. A zero
// field is unknown.
type ServerIdentity struct {
	StartedAt  time.Time
	SystemID   string
	TimelineID int64
	Role       string // ServerRolePrimary, ServerRoleStandby or "" (unknown)
	VersionNum int64
}

// IsZero reports whether nothing about the server is known.
func (s ServerIdentity) IsZero() bool { return s == ServerIdentity{} }

// IdentityOf decodes the identity columns of a row. Missing, null or
// malformed values are unknown, never coerced.
func IdentityOf(r Row) ServerIdentity {
	if r == nil {
		return ServerIdentity{}
	}
	id := ServerIdentity{StartedAt: timeField(r, "server_started_at"),
		SystemID: systemID(r["system_identifier"]), TimelineID: positiveInt(r, "timeline_id"),
		VersionNum: positiveInt(r, "server_version_num")}
	if b, ok := r["in_recovery"].(bool); ok {
		id.Role = ServerRolePrimary
		if b {
			id.Role = ServerRoleStandby
		}
	}
	return id
}

func systemID(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return x.String()
		}
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}

func positiveInt(r Row, key string) int64 {
	n, err := intField(r, key)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
