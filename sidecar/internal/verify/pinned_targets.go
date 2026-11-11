package verify

import (
	"context"
	"errors"
	"fmt"
)

// maxOpenTargetIDs bounds one read of the open targets; the collector
// pins at most 64 of them a cycle (collection-07).
const maxOpenTargetIDs = 256

// openTargetIDsSQL lists the target queryids of the verifications still
// open, oldest verification first, each once. A criterion whose TargetIDs
// is not an array, and elements that are not int8 integers (a number past
// the int8 range included), are skipped rather than failing the read: one
// bad row must not drop every other watch's pins. The CASE keeps the
// numeric cast behind the pattern, whatever order the quals run in.
const openTargetIDsSQL = `SELECT t.queryid FROM (
	SELECT e.value::int8 AS queryid, min(v.id) AS first_watch
	  FROM sage.verification v
	 CROSS JOIN LATERAL jsonb_array_elements_text(
	       CASE WHEN jsonb_typeof(v.criterion->'TargetIDs') = 'array'
	            THEN v.criterion->'TargetIDs' ELSE '[]'::jsonb END) AS e(value)
	 WHERE v.completed_at IS NULL
	   AND CASE WHEN e.value ~ '^-?[0-9]{1,19}$'
	            THEN e.value::numeric BETWEEN -9223372036854775808 AND 9223372036854775807
	            ELSE false END
	 GROUP BY 1) t
 ORDER BY t.first_watch, t.queryid
 LIMIT $1`

// OpenTargetIDs is the target queryids of the open verifications, oldest
// first: the collector samples them whatever their rank, so a target that
// leaves the top-N after its index still has its evidence
// (collection-07). It reads sage.verification, so the pins survive a
// restart.
func (s *PostgresStateStore) OpenTargetIDs(ctx context.Context) ([]int64, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("verify state pool is unavailable")
	}
	rows, err := s.pool.Query(ctx, openTargetIDsSQL, maxOpenTargetIDs)
	if err != nil {
		return nil, fmt.Errorf("read open verification targets: %w", err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan open verification target: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open verification targets: %w", err)
	}
	return ids, nil
}
