package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// collectForeignKeys reads one ForeignKey per constraint. No keys is an
// empty list, stored as [] (a real "none"), not null.
func (c *Collector) collectForeignKeys(ctx context.Context) ([]ForeignKey, error) {
	rows, err := c.catalogQuery(ctx, foreignKeysSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []ForeignKey{}
	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(
			&fk.Schema, &fk.TableName, &fk.RefSchema, &fk.ReferencedTable,
			&fk.ConstraintName, &fk.OID, &fk.Columns,
		); err != nil {
			return nil, fmt.Errorf("scan foreign key: %w", err)
		}
		result = append(result, fk)
	}
	return result, rows.Err()
}

// storedForeignKey is a stored foreign_keys element of either shape:
// FKColumn is set only in rows written before collection-04, which held
// one element per referencing column and no schema.
type storedForeignKey struct {
	ForeignKey
	FKColumn *string `json:"fk_column"`
}

// DecodeForeignKeys decodes a stored foreign_keys document. A legacy
// document (one element per column) is regrouped into one ForeignKey per
// (table, constraint), columns in row order and each listed once, with an
// empty Schema and RefSchema: the owning schema of such a row is unknown. null and [] are
// no keys.
func DecodeForeignKeys(raw []byte) ([]ForeignKey, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var stored []storedForeignKey
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decode foreign_keys document: %w", err)
	}
	out := make([]ForeignKey, 0, len(stored))
	legacy := map[[2]string]int{}
	for _, s := range stored {
		if s.FKColumn == nil {
			out = append(out, s.ForeignKey)
			continue
		}
		key := [2]string{s.TableName, s.ConstraintName}
		i, seen := legacy[key]
		if !seen {
			i = len(out)
			legacy[key] = i
			out = append(out, ForeignKey{TableName: s.TableName,
				ReferencedTable: s.ReferencedTable, ConstraintName: s.ConstraintName})
		}
		// One tenant's (table, constraint) repeats another's: the rows
		// cannot be told apart, so a column is listed once.
		if !slices.Contains(out[i].Columns, *s.FKColumn) {
			out[i].Columns = append(out[i].Columns, *s.FKColumn)
		}
	}
	return out, nil
}
