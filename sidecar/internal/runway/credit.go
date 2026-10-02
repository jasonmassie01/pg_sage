package runway

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Avoided-incident credit for a WAL bound (LEDGER "Avoided-incident
// credit", disk_full_slot). A bound caps the WAL slots may retain; it
// avoided an incident only if the disk was measured to be filling toward
// its declared capacity inside the horizon before the action, and the
// bounded projection after it no longer reaches capacity. Used bytes are
// the databases plus pg_wal (other files on the volume are not seen, so
// free space is overestimated: the credit is conservative). Managed
// providers are never credited: free space is not measurable from SQL
// there and storage may grow on its own.

// DiskMeasure is one measurement of the disk runway.
type DiskMeasure struct {
	UsedBytes     float64
	RetainedBytes float64
	Disk          probes.RunwayTrend
	Databases     probes.RunwayTrend
	TrendsOK      bool
}

// CreditInput is everything WALBoundCredit decides on.
type CreditInput struct {
	Capacity        float64
	ManagedProvider bool
	Before, After   DiskMeasure
	// BoundBytes is max_slot_wal_keep_size after the action (-1 unbounded).
	BoundBytes float64
	Horizon    time.Duration
	MinSamples int
	MinSpan    time.Duration
}

// WALBoundCredit decides whether a verified WAL bound avoided a disk-full
// incident, with the reason either way.
func WALBoundCredit(c CreditInput) (bool, string) {
	opts := Options{MinSamples: c.MinSamples, MinSpan: c.MinSpan}
	disk, dbs := c.Before.Disk, c.Before.Databases
	switch {
	case c.ManagedProvider:
		return false, "managed provider: free space is not measurable from SQL"
	case !(c.Capacity > 0):
		return false, "disk capacity is not declared"
	case !c.Before.TrendsOK || !measured(disk, opts) || !(disk.R2 >= levelMinR2):
		return false, "no steady disk fill trend was measured before the action"
	case !(disk.RatePerS > 0):
		return false, "the disk was not filling before the action"
	case !measured(dbs, opts):
		return false, "database growth was not measured before the action"
	case !(c.Before.UsedBytes > 0) || !(c.After.UsedBytes > 0):
		return false, "disk usage was not measured on both sides of the action"
	case !(c.BoundBytes >= 0):
		return false, "the WAL bound is not in effect"
	}
	before := (c.Capacity - c.Before.UsedBytes) / disk.RatePerS
	if !within(before, c.Horizon) {
		return false, fmt.Sprintf("the trend reached capacity in %s s, beyond the "+
			"%s s horizon", probes.FormatValue(round2(before)),
			probes.FormatValue(c.Horizon.Seconds()))
	}
	walGrowth := math.Max(0, c.BoundBytes-math.Max(0, c.After.RetainedBytes))
	dbGrowth := math.Max(0, dbs.RatePerS) * c.Horizon.Seconds()
	projected := c.After.UsedBytes + walGrowth + dbGrowth
	if !(projected < c.Capacity) {
		return false, fmt.Sprintf("with the bound, usage still reaches %s of %s bytes "+
			"within the horizon", probes.FormatValue(math.Round(projected)),
			probes.FormatValue(c.Capacity))
	}
	return true, fmt.Sprintf("disk full projected in %s s before the action; with WAL "+
		"bounded at %s bytes usage peaks at %s of %s bytes within %s s",
		probes.FormatValue(round2(before)), probes.FormatValue(c.BoundBytes),
		probes.FormatValue(math.Round(projected)), probes.FormatValue(c.Capacity),
		probes.FormatValue(c.Horizon.Seconds()))
}

// MeasureDisk measures used bytes (databases plus pg_wal), the WAL the
// largest slot retains and the sampled disk and database trends over
// window. An unreadable probe is an error: nothing is measured.
func MeasureDisk(ctx context.Context, r ProbeRunner, window time.Duration) (DiskMeasure,
	error) {
	var m DiskMeasure
	wal, err := probes.WALRunwayOf(r.Run(ctx, probes.WALRunwayProbe, probes.Args{}))
	if err != nil {
		return m, fmt.Errorf("measure disk: wal_runway: %w", err)
	}
	dir, err := probes.WALDirectoryOf(r.Run(ctx, probes.WALDirectoryProbe, probes.Args{}))
	if err != nil {
		return m, fmt.Errorf("measure disk: wal_directory: %w", err)
	}
	if !probes.Known(wal.DatabaseBytes) || wal.UnreadableDatabases != 0 {
		return m, fmt.Errorf("measure disk: database sizes are not all readable")
	}
	slots, err := probes.Slots(r.Run(ctx, probes.ReplicationSlots, probes.Args{}))
	if err != nil {
		return m, fmt.Errorf("measure disk: replication_slots: %w", err)
	}
	m.UsedBytes = wal.DatabaseBytes + dir.Bytes
	for _, s := range slots {
		if probes.Known(s.RetainedBytes) {
			m.RetainedBytes = math.Max(m.RetainedBytes, s.RetainedBytes)
		}
	}
	ts, err := probes.RunwayTrends(r.Run(ctx, probes.RunwayTrendsProbe,
		probes.Args{Window: window}))
	if err == nil {
		disk, okDisk := probes.FindTrend(ts, probes.RunwayDiskUsed, probes.SubjectCluster)
		dbs, okDB := probes.FindTrend(ts, probes.RunwayDatabaseBytes, probes.SubjectCluster)
		m.Disk, m.Databases, m.TrendsOK = disk, dbs, okDisk && okDB
	}
	return m, nil
}
