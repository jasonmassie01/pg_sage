package autonomy

import (
	"context"
	"errors"
	"maps"
	"math"
)

// Where the WAL custodian's disk capacity came from (clouds-07), recorded
// in its bound proposals' evidence. On managed providers PostgreSQL cannot
// see its volume; managed-cloud telemetry measures it.
const (
	CapacitySourceProvider  = "provider_telemetry"
	CapacitySourceUnbounded = "capacity_unbounded"
)

// ErrDiskCapacityUnbounded is a DiskCapacityProvider's answer for storage
// that grows without a bound (Aurora, Cloud SQL auto-resize without a
// limit): there is no disk percentage, and the custodian says so.
var ErrDiskCapacityUnbounded = errors.New("disk capacity is unbounded")

// walCapacity is one scan's disk capacity. Without a provider, or with an
// unavailable, stale or non-positive one, it is unknown and source is
// empty: the custodian then decides exactly as it did without telemetry.
type walCapacity struct {
	bytes  int64
	known  bool
	source string
}

func (c *PostgresWALCustodian) diskCapacity(ctx context.Context) walCapacity {
	if c.options.Disk == nil {
		return walCapacity{}
	}
	bytes, err := c.options.Disk.CapacityBytes(ctx)
	switch {
	case errors.Is(err, ErrDiskCapacityUnbounded):
		return walCapacity{source: CapacitySourceUnbounded}
	case err != nil || bytes <= 0:
		return walCapacity{}
	}
	return walCapacity{bytes: bytes, known: true, source: CapacitySourceProvider}
}

// knownCapacity is a capacity the caller measured (scanSlot).
func knownCapacity(bytes int64, known bool) walCapacity {
	if !known || bytes <= 0 {
		return walCapacity{}
	}
	return walCapacity{bytes: bytes, known: true, source: CapacitySourceProvider}
}

// withCapacityEvidence records the capacity a bound or escalation was
// decided with. Without provider capacity the proposal is unchanged.
func withCapacityEvidence(p Proposal, slot walSlot, capacity walCapacity) Proposal {
	if capacity.source == "" || (p.SQL == "" && p.Plan == "") {
		return p
	}
	evidence := make(map[string]any, len(p.Evidence)+4)
	maps.Copy(evidence, p.Evidence)
	evidence["capacity_source"] = capacity.source
	evidence["retained_wal_bytes"] = slot.retained
	if capacity.known {
		evidence["disk_capacity_bytes"] = capacity.bytes
		evidence["disk_pct"] = math.Round(
			float64(slot.retained)*10000/float64(capacity.bytes)) / 100
	}
	p.Evidence = evidence
	return p
}
