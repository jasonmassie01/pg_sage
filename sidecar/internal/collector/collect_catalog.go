package collector

import (
	"context"
)

func (c *Collector) collectForeignKeys(ctx context.Context) ([]ForeignKey, error) {
	rows, err := c.catalogQuery(ctx, foreignKeysSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ForeignKey
	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(
			&fk.TableName, &fk.ReferencedTable,
			&fk.FKColumn, &fk.ConstraintName,
		); err != nil {
			return nil, err
		}
		result = append(result, fk)
	}
	return result, rows.Err()
}
