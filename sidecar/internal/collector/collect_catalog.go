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

func (c *Collector) collectLocks(ctx context.Context) ([]LockInfo, error) {
	rows, err := c.catalogQuery(ctx, locksSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []LockInfo
	for rows.Next() {
		var lk LockInfo
		if err := rows.Scan(
			&lk.LockType, &lk.Mode, &lk.Granted,
			&lk.RelName,
			&lk.Query, &lk.State,
			&lk.WaitEventType, &lk.WaitEvent,
			&lk.PID,
			&lk.BackendStart, &lk.QueryStart,
		); err != nil {
			return nil, err
		}
		result = append(result, lk)
	}
	return result, rows.Err()
}
