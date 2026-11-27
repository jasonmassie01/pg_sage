package collector

import (
	"context"
	"fmt"
)

// partitionedIndexesSQL reads the partitioned (relkind I) indexes, which
// pg_stat_user_indexes leaves out (rules-03): without them every key of a
// partitioned table looks unindexed and an unused partitioned index can
// only be seen as its attached children, which PostgreSQL refuses to drop.
// Keyset-paged by oid. Key columns are in key order without INCLUDE
// columns; an expression key is the empty string.
const partitionedIndexesSQL = sageTag + `
SELECT n.nspname, tc.relname, ic.relname, ic.oid,
       COALESCE((SELECT inh.inhparent FROM pg_inherits inh
                  WHERE inh.inhrelid = ic.oid), 0::oid) AS parent_index_oid,
       ARRAY(SELECT COALESCE(a.attname::text, '')
               FROM generate_series(0, ix.indnkeyatts - 1) AS k(i)
               LEFT JOIN pg_attribute a
                 ON a.attrelid = ix.indrelid AND a.attnum = ix.indkey[k.i]
              ORDER BY k.i) AS key_columns,
       ix.indisvalid, ix.indisunique, ix.indisprimary,
       ix.indpred IS NOT NULL AS partial
  FROM pg_class ic
  JOIN pg_index ix ON ix.indexrelid = ic.oid
  JOIN pg_class tc ON tc.oid = ix.indrelid
  JOIN pg_namespace n ON n.oid = tc.relnamespace
 WHERE ic.relkind = 'I'
   AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')
   AND n.nspname !~ '^pg_toast'
   AND ic.oid > $1
 ORDER BY ic.oid
 LIMIT $2`

// collectPartitionedIndexes pages through the partitioned indexes by oid.
func (c *Collector) collectPartitionedIndexes(ctx context.Context) ([]PartitionedIndex, error) {
	batchSize := collectorBatchSize(c.cfg)
	var result []PartitionedIndex
	var cursor uint32
	for {
		batch, err := c.readPartitionedIndexPage(ctx, cursor, batchSize)
		if err != nil {
			return nil, fmt.Errorf("collect partitioned indexes: %w", err)
		}
		result = append(result, batch...)
		if len(batch) < batchSize {
			return result, nil
		}
		cursor = batch[len(batch)-1].IndexRelID
	}
}

func (c *Collector) readPartitionedIndexPage(
	ctx context.Context, cursor uint32, batchSize int,
) ([]PartitionedIndex, error) {
	rows, err := c.catalogQuery(ctx, partitionedIndexesSQL, cursor, batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartitionedIndex
	for rows.Next() {
		var pi PartitionedIndex
		if err := rows.Scan(&pi.SchemaName, &pi.RelName, &pi.IndexRelName,
			&pi.IndexRelID, &pi.ParentIndexOID, &pi.KeyColumns,
			&pi.IsValid, &pi.IsUnique, &pi.IsPrimary, &pi.IsPartial); err != nil {
			return nil, err
		}
		out = append(out, pi)
	}
	return out, rows.Err()
}
