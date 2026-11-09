package analyzer

import "github.com/pg-sage/sidecar/internal/collector"

// fkTables resolves where a foreign key is enforced. The collector reads
// one row per constraint, the partitioned parent's, and leaves out its
// per-partition clones (collection-04). PostgreSQL enforces such a key on
// every leaf partition, and pg_stat_user_indexes lists only the leaves'
// indexes, never the partitioned parent's, so the key is checked, and its
// supporting indexes protected, on the leaves.
type fkTables struct {
	kind     map[tableKey]string
	children map[tableKey][]tableKey
}

func newFKTables(snap *collector.Snapshot) *fkTables {
	t := &fkTables{kind: make(map[tableKey]string, len(snap.Tables)),
		children: make(map[tableKey][]tableKey)}
	for _, ts := range snap.Tables {
		t.kind[tableKey{ts.SchemaName, ts.RelName}] = ts.Relkind
	}
	for _, p := range snap.Partitions {
		parent := tableKey{p.ParentSchema, p.ParentTable}
		t.children[parent] = append(t.children[parent],
			tableKey{p.ChildSchema, p.ChildTable})
	}
	return t
}

// leaves returns the tables whose indexes support a key on table: table
// itself, or for a partitioned table every leaf partition in the
// snapshot. A table missing from the snapshot, or a partitioned table
// whose partitions are unknown, has none (nothing is guessed).
func (t *fkTables) leaves(table tableKey) []tableKey {
	kind, known := t.kind[table]
	if !known {
		return nil
	}
	if kind != "p" {
		return []tableKey{table}
	}
	var out []tableKey
	seen := map[tableKey]bool{table: true}
	queue := []tableKey{table}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range t.children[cur] {
			childKind, ok := t.kind[child]
			if !ok || seen[child] {
				continue
			}
			seen[child] = true
			if childKind == "p" {
				queue = append(queue, child)
			} else {
				out = append(out, child)
			}
		}
	}
	return out
}

// supportTables returns the tables whose indexes may support a key on
// table, for protection from unused-index drops. Unlike leaves it
// guesses in the safe direction: table itself unless it is known to be
// partitioned, and every partition below it, whether or not the
// partition is in the snapshot's table list.
func (t *fkTables) supportTables(table tableKey) []tableKey {
	var out []tableKey
	seen := map[tableKey]bool{table: true}
	queue := []tableKey{table}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if t.kind[cur] != "p" {
			out = append(out, cur)
		}
		for _, child := range t.children[cur] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	return out
}

// fkKeysPlaced reports whether every key's supporting tables are known:
// the partition list was read, or each key is on a table known not to be
// partitioned. A key on a partitioned (or unknown) table whose partitions
// could not be read leaves its supporting indexes unknown.
func fkKeysPlaced(snap *collector.Snapshot) bool {
	if snap.Available("partitions") {
		return true
	}
	kinds := newFKTables(snap).kind
	for _, fk := range snap.ForeignKeys {
		kind, known := kinds[tableKey{fk.Schema, fk.TableName}]
		if !known || kind == "p" {
			return false
		}
	}
	return true
}
