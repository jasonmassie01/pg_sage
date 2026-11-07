package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// xminHoldersSQL is the shared xmin_horizon query (this database's
// sessions by snapshot or XID, standbys' feedback, its prepared
// transactions, slots' xmin and catalog_xmin), oldest first. The RCA
// vacuum_blocked tree names its holder from these rows (rca-01).
var xminHoldersSQL = probes.XminHorizonSQL

// xminHolderLimit bounds the holders read per cycle.
const xminHolderLimit = 20

// collectXminHoldersInto reads the xmin holders into snap. A failed read
// marks the category unavailable (unknown, not "no holders"); the
// snapshot is kept.
func (c *Collector) collectXminHoldersInto(ctx context.Context, snap *Snapshot) {
	holders, err := c.collectXminHolders(ctx)
	if err == nil {
		snap.XminHolders = holders
		return
	}
	c.logFn("WARN", "collector: xmin holders unavailable this cycle (vacuum_blocked "+
		"names no holder): %v", err)
	if snap.Unavailable == nil {
		snap.Unavailable = map[string]string{}
	}
	snap.Unavailable["xmin_holders"] = err.Error()
}

func (c *Collector) collectXminHolders(ctx context.Context) ([]probes.XminHolder, error) {
	rows, err := c.catalogQuery(ctx, xminHoldersSQL, xminHolderLimit)
	if err != nil {
		return nil, fmt.Errorf("read xmin holders: %w", err)
	}
	defer rows.Close()
	holders := []probes.XminHolder{}
	for rows.Next() {
		h, err := scanXminHolder(rows)
		if err != nil {
			return nil, err
		}
		holders = append(holders, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read xmin holders: %w", err)
	}
	return holders, nil
}

// scanXminHolder scans one xmin_horizon row. NULL columns read as zero
// values (never NaN: an unknown age pins nothing, and the snapshot must
// marshal).
func scanXminHolder(row pgx.Row) (probes.XminHolder, error) {
	var h probes.XminHolder
	var pid, age *int64
	var start *time.Time
	var name, state, app *string
	var xactAge *float64
	var active *bool
	if err := row.Scan(&h.Kind, &pid, &start, &name, &state, &age, &xactAge, &app,
		&active); err != nil {
		return probes.XminHolder{}, fmt.Errorf("scan xmin holder: %w", err)
	}
	h.PID = orZero(pid)
	h.BackendStart = orZero(start)
	h.Name, h.State, h.Application = orZero(name), orZero(state), orZero(app)
	h.XminAge = float64(orZero(age))
	h.XactAgeS = orZero(xactAge)
	h.Active = orZero(active)
	return h, nil
}

func orZero[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
