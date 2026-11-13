package collector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// CDCHeartbeatReading is one replication.cdc_heartbeats read: the newest
// value of the heartbeat column. Latest is nil when the table has no row;
// Err is set when the read failed, which leaves the heartbeat unknown.
type CDCHeartbeatReading struct {
	Slot   string
	Table  string
	Column string
	MaxAge time.Duration
	Latest *time.Time
	Err    string
}

// heartbeatStatementTimeout bounds each heartbeat read (market-13).
const heartbeatStatementTimeout = 2 * time.Second

// heartbeats returns the configured heartbeats (nil without a config).
func (c *Collector) heartbeats() []config.CDCHeartbeat {
	if c.cfg == nil {
		return nil
	}
	return c.cfg.Replication.CDCHeartbeats
}

// heartbeatTimeouts are the configured catalog timeouts with the statement
// timeout capped at 2 s and the lock timeout never above it.
func heartbeatTimeouts(cfg *config.Config) catalogread.Timeouts {
	t := catalogread.FromSafety(cfg.Safety)
	if t.Statement <= 0 || t.Statement > heartbeatStatementTimeout {
		t.Statement = heartbeatStatementTimeout
	}
	if t.Lock <= 0 || t.Lock > t.Statement {
		t.Lock = t.Statement
	}
	return t
}

// cdcHeartbeatSQL is the one read of a heartbeat: table is "name" or
// "schema.name" (config load admits plain identifiers only); each part is
// quoted again here, so no name is ever interpolated as SQL. A plain
// identifier means what it means unquoted in SQL, so it is folded to lower
// case before quoting: CDC.Heartbeat reads cdc.heartbeat.
func cdcHeartbeatSQL(table, column string) (string, error) {
	parts := strings.Split(table, ".")
	if len(parts) > 2 || column == "" {
		return "", fmt.Errorf("heartbeat %q.%q: want table or schema.table and a column",
			table, column)
	}
	for i, p := range parts {
		if p == "" {
			return "", fmt.Errorf("heartbeat table %q has an empty name part", table)
		}
		parts[i] = strings.ToLower(p)
	}
	col := pgx.Identifier{strings.ToLower(column)}.Sanitize()
	return sageTag + "SELECT max(" + col + ")::timestamptz FROM " +
		pgx.Identifier(parts).Sanitize(), nil
}

// collectCDCHeartbeats reads each configured heartbeat whose slot is a
// logical slot of this database. One that cannot be placed (the slot list
// was not read) or read is reported with Err, never dropped.
func (c *Collector) collectCDCHeartbeats(ctx context.Context,
	snap *Snapshot) []CDCHeartbeatReading {
	local := map[string]bool{}
	if snap.Replication != nil {
		for _, s := range snap.Replication.Slots {
			local[s.SlotName] = s.SlotType == "logical" && s.InCurrentDatabase
		}
	}
	slotsKnown := snap.Available("replication")
	var out []CDCHeartbeatReading
	for _, hb := range c.heartbeats() {
		r := CDCHeartbeatReading{Slot: hb.Slot, Table: hb.Table, Column: hb.Column,
			MaxAge: time.Duration(hb.MaxAgeMinutes) * time.Minute}
		switch {
		case !slotsKnown:
			r.Err = "the replication slot list could not be read this cycle"
		case !local[hb.Slot]:
			continue
		default:
			r.Latest, r.Err = c.readHeartbeat(ctx, hb)
		}
		out = append(out, r)
	}
	return out
}

// readHeartbeat runs one bounded, read-only SELECT max(column).
func (c *Collector) readHeartbeat(ctx context.Context, hb config.CDCHeartbeat) (*time.Time,
	string) {
	sql, err := cdcHeartbeatSQL(hb.Table, hb.Column)
	if err != nil {
		return nil, err.Error()
	}
	r := c.reader(c.pool)
	r.Timeouts = heartbeatTimeouts(c.cfg)
	var latest *time.Time
	if err := r.QueryRow(ctx, sql).Scan(&latest); err != nil {
		c.logFn("WARN", "collector: CDC heartbeat %s (%s.%s) unread: %v",
			hb.Slot, hb.Table, hb.Column, err)
		return nil, err.Error()
	}
	return latest, ""
}
