package collector

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func (c *Collector) collectReplication(
	ctx context.Context,
) (*ReplicationStats, error) {
	rs := &ReplicationStats{}

	replicaRows, err := c.catalogQuery(ctx, replicationReplicasSQL)
	if err != nil {
		return nil, err
	}
	if rs.Replicas, err = scanReplicaRows(replicaRows); err != nil {
		return nil, err
	}

	slotRows, err := c.catalogQuery(ctx, replicationSlotsSQL)
	if err != nil {
		return nil, err
	}
	if rs.Slots, err = scanSlotRows(slotRows); err != nil {
		return nil, err
	}

	// Return nil if no replication data exists.
	if len(rs.Replicas) == 0 && len(rs.Slots) == 0 {
		return nil, nil
	}
	return rs, nil
}

// scanReplicaRows reads pg_stat_replication rows. LSN columns are NULL
// while a walsender starts up or catches up; they scan into nil instead
// of failing the category and dropping the slots too (G1-B20).
func scanReplicaRows(rows pgx.Rows) ([]ReplicaInfo, error) {
	defer rows.Close()
	var replicas []ReplicaInfo
	for rows.Next() {
		var r ReplicaInfo
		if err := rows.Scan(
			&r.ClientAddr, &r.State,
			&r.SentLSN, &r.WriteLSN, &r.FlushLSN, &r.ReplayLSN,
			&r.WriteLag, &r.FlushLag, &r.ReplayLag,
			&r.SyncState,
		); err != nil {
			return nil, err
		}
		replicas = append(replicas, r)
	}
	return replicas, rows.Err()
}

func scanSlotRows(rows pgx.Rows) ([]SlotInfo, error) {
	defer rows.Close()
	var slots []SlotInfo
	for rows.Next() {
		var s SlotInfo
		if err := rows.Scan(
			&s.SlotName, &s.SlotType, &s.Active, &s.RetainedBytes,
		); err != nil {
			return nil, err
		}
		slots = append(slots, s)
	}
	return slots, rows.Err()
}

func (c *Collector) collectIO(ctx context.Context) ([]IOStats, error) {
	rows, err := c.catalogQuery(ctx, ioStatsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []IOStats
	for rows.Next() {
		var s IOStats
		if err := rows.Scan(
			&s.BackendType, &s.Object, &s.Context,
			&s.Reads, &s.ReadTime,
			&s.Writes, &s.WriteTime,
			&s.Writebacks, &s.WritebackTime,
			&s.Extends, &s.ExtendTime,
			&s.Hits, &s.Evictions,
			&s.Reuses, &s.Fsyncs,
			&s.FsyncTime,
		); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (c *Collector) collectPartitions(
	ctx context.Context,
) ([]PartitionInfo, error) {
	rows, err := c.catalogQuery(ctx, partitionInheritanceSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PartitionInfo
	for rows.Next() {
		var p PartitionInfo
		if err := rows.Scan(
			&p.ChildTable, &p.ChildSchema,
			&p.ParentTable, &p.ParentSchema,
		); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (c *Collector) collectPreparedXacts(
	ctx context.Context,
) ([]PreparedTransaction, error) {
	rows, err := c.catalogQuery(ctx, preparedXactsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PreparedTransaction
	for rows.Next() {
		var pt PreparedTransaction
		if err := rows.Scan(
			&pt.GID, &pt.Prepared, &pt.Owner,
			&pt.Database, &pt.XIDAge,
		); err != nil {
			return nil, err
		}
		result = append(result, pt)
	}
	return result, rows.Err()
}
