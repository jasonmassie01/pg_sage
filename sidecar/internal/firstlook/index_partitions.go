package firstlook

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// maxAttachListed bounds the partitions one invalid partitioned index
// names and suggests SQL for.
const maxAttachListed = 20

// unattachedSQL lists, for each invalid partitioned index in $1, the
// direct partitions of its table with no index attached to it: the first
// $2 by name, the total and the index's definition. It reads the catalog
// only, for the flagged indexes only.
const unattachedSQL = tag + `SELECT s.parent, s.schema, s.name, s.partitioned, s.total, s.def
FROM (
  SELECT x.indexrelid AS parent, n.nspname::text AS schema, c.relname::text AS name,
    c.relkind = 'p' AS partitioned,
    count(*) OVER (PARTITION BY x.indexrelid)::int AS total,
    row_number() OVER (PARTITION BY x.indexrelid ORDER BY n.nspname, c.relname) AS rn,
    pg_catalog.pg_get_indexdef(x.indexrelid) AS def
  FROM pg_catalog.pg_index x
  JOIN pg_catalog.pg_inherits ht ON ht.inhparent = x.indrelid
  JOIN pg_catalog.pg_class c ON c.oid = ht.inhrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE x.indexrelid = ANY($1::oid[])
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits hi
                    JOIN pg_catalog.pg_index ci ON ci.indexrelid = hi.inhrelid
                    WHERE hi.inhparent = x.indexrelid AND ci.indrelid = c.oid)
) s
WHERE s.rn <= $2
ORDER BY s.parent, s.rn`

// readUnattached names, for each invalid partitioned index in idx, the
// partitions it is missing, so the rule can suggest the indexes to attach.
// It sends nothing when there are none; on failure no index is marked read.
func readUnattached(ctx context.Context, tx pgx.Tx, idx []Index) error {
	pos := map[uint32]int{}
	var oids []uint32
	for i, x := range idx {
		if x.Partitioned && !x.Valid {
			pos[x.OID] = i
			oids = append(oids, x.OID)
		}
	}
	if len(oids) == 0 {
		return nil
	}
	found := map[uint32][]Partition{}
	totals, defs := map[uint32]int{}, map[uint32]string{}
	err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
		rows, err := sp.Query(ctx, unattachedSQL, oids, maxAttachListed)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var oid uint32
			var p Partition
			var total int
			var def string
			if err := rows.Scan(&oid, &p.Schema, &p.Name, &p.Partitioned, &total,
				&def); err != nil {
				return err
			}
			found[oid] = append(found[oid], p)
			totals[oid], defs[oid] = total, def
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("read unattached partitions of %d invalid partitioned indexes: %w",
			len(oids), err)
	}
	for oid, i := range pos {
		idx[i].Unattached, idx[i].UnattachedTotal = found[oid], totals[oid]
		idx[i].IndexDef, idx[i].UnattachedRead = defs[oid], true
	}
	return nil
}
